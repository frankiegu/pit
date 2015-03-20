package recommender

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/alonsovidales/pit/adaptative_bootstrap_tree"
	"github.com/alonsovidales/pit/log"
	"github.com/goamz/goamz/aws"
	"github.com/goamz/goamz/s3"
	"io/ioutil"
	"strings"
	"sync"
	"time"
)

const (
	STATUS_SARTING       = "STARTING"
	STATUS_ACTIVE        = "ACTIVE"
	STATUS_NORECORDS     = "NO_RECORDS"
	STATUS_RECALCULATING = "RECALCULATING"

	cMinRecordsToStart = 100
	cRecTreeMaxDeep    = 30
	cRecTreeNumOfTrees = 10

	S3BUCKET = "pit-backups"
)

type RecommenderInt interface {
	// Calculates the scores for the given records, and stores in memory
	// the classification for further processing
	CalcScores(recID uint64, scores map[uint64]uint64) (result []uint64)
	// Just adds a new record to the recommender system in order to
	// increase the knoledge DB
	AddRecord(recID uint64, scores map[uint64]uint64)
	// CalcScores Lanches the ETL process to create the tree
	RecalculateTree()
	// Stores all the records serialized in a inexpensive storage system
	SaveBackup()
	// Restore from backup
	LoadBackup() (success bool)
	// GetStatus Returns the current status of this recommender system,
	// the posible statuses can be: ACTIVE, STARTING, NO_RECORDS
	GetStatus() string
}

type score struct {
	recID  uint64
	scores map[uint64]uint8
	next   *score
	prev   *score
}

type Recommender struct {
	RecommenderInt

	identifier string
	maxScore   uint8
	s3Path     string
	s3Region   aws.Region

	maxClassif   uint64
	totalClassif uint64

	status string

	records map[uint64]*score
	older   *score
	newer   *score

	recTree rectree.BoostrapRecTree

	mutex sync.Mutex
}

func NewShard(s3Path string, identifier string, maxClassif uint64, maxScore uint8, s3Region string) (rc *Recommender) {
	log.Info("Starting shard:", identifier, "With max number of elements:", maxClassif)

	rc = &Recommender{
		identifier:   identifier,
		maxClassif:   maxClassif,
		totalClassif: 0,
		maxScore:     maxScore,
		records:      make(map[uint64]*score),
		status:       STATUS_SARTING,
		s3Path:       s3Path,
		s3Region:     aws.Regions[s3Region],
	}

	go rc.checkAndExpire()

	return
}

func (rc *Recommender) GetStatus() string {
	return rc.status
}

func (rc *Recommender) CalcScores(recID uint64, scores map[uint64]uint8, maxToReturn int) (result []uint64) {
	if rc.recTree == nil {
		log.Debug("No tree:'(")
		return
	}

	rc.AddRecord(recID, scores)
	result = rc.recTree.GetBestRecommendation(scores, maxToReturn)

	return
}

func (rc *Recommender) AddRecord(recID uint64, scores map[uint64]uint8) {
	//log.Debug("Adding:", recID, "Scores:", scores)
	var sc *score
	var existingRecord bool
	rc.mutex.Lock()
	if sc, existingRecord = rc.records[recID]; existingRecord {
		sc.prev.next = sc.next
		sc.next.prev = sc.prev

		rc.totalClassif += uint64(len(scores) - len(sc.scores))
		sc.scores = scores
	} else {
		sc = &score{
			recID:  recID,
			scores: scores,
		}
		rc.records[recID] = sc
		rc.totalClassif += uint64(len(scores))
	}

	if rc.newer != nil {
		sc.prev = rc.newer
		rc.newer.next = sc

		rc.newer.next = sc
		rc.newer = sc
	} else {
		rc.newer = sc
		rc.older = sc
	}
	rc.mutex.Unlock()
}

func (rc *Recommender) RecalculateTree() {
	if len(rc.records) < cMinRecordsToStart {
		rc.status = STATUS_NORECORDS
		return
	}

	rc.status = STATUS_RECALCULATING
	rc.mutex.Lock()
	records := make([]map[uint64]uint8, len(rc.records))
	i := 0
	for _, record := range rc.records {
		records[i] = record.scores
		i++
	}
	rc.mutex.Unlock()

	rc.status = STATUS_ACTIVE

	rc.recTree = rectree.ProcessNewTrees(records, cRecTreeMaxDeep, rc.maxScore, cRecTreeNumOfTrees)
}

func (rc *Recommender) LoadBackup() (success bool) {
	log.Debug("Loading backup from S3...")
	auth, err := aws.EnvAuth()
	if err != nil {
		log.Error("Problem trying to connect with AWS:", err)
		return false
	}

	s := s3.New(auth, rc.s3Region)
	bucket := s.Bucket(S3BUCKET)

	jsonData, err := bucket.Get(rc.getS3Path())
	if err != nil {
		log.Info("Problem trying to get backup from S3:", err)
		return false
	}

	dataFromJson := [][]uint64{}
	json.Unmarshal(rc.uncompress(jsonData), &dataFromJson)

	log.Debug("Load2", len(dataFromJson))
	recs := 0
	for _, record := range dataFromJson {
		scores := make(map[uint64]uint8)
		for i := 1; i < len(record); i += 2 {
			scores[record[i]] = uint8(record[i+1])
		}
		recs += len(scores)
		rc.AddRecord(record[0], scores)
	}

	return true
}

func (rc *Recommender) SaveBackup() {
	log.Debug("Storing backup on S3...")
	rc.mutex.Lock()
	records := make([][]uint64, len(rc.records))
	i := 0
	for recID, record := range rc.records {
		records[i] = make([]uint64, len(record.scores)*2+1)
		records[i][0] = recID
		elemPos := 1
		for k, v := range record.scores {
			records[i][elemPos] = k
			records[i][elemPos+1] = uint64(v)
			elemPos += 2
		}
		i++
	}
	rc.mutex.Unlock()

	jsonToUpload, err := json.Marshal(records)

	auth, err := aws.EnvAuth()
	if err != nil {
		log.Error("Problem trying to connect with AWS:", err)
		return
	}

	s := s3.New(auth, rc.s3Region)
	bucket := s.Bucket(S3BUCKET)

	err = bucket.Put(
		rc.getS3Path(),
		rc.compress(jsonToUpload),
		"text/plain",
		s3.BucketOwnerFull,
		s3.Options{})
	if err != nil {
		log.Error("Problem trying to upload backup to S3 from:", rc.identifier, "Error:", err)
	}
}

func (rc *Recommender) getS3Path() string {
	return fmt.Sprintf("%s/%s.json.gz", rc.s3Path, rc.identifier)
}

func (rc *Recommender) uncompress(data []byte) (result []byte) {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		log.Debug("The data can't be uncompressed on shard:", rc.identifier, "Error:", err)
	}
	defer gz.Close()
	if result, err = ioutil.ReadAll(gz); err != nil {
		log.Debug("The data can't be uncompressed on shard:", rc.identifier, "Error:", err)
	}

	return
}

func (rc *Recommender) compress(data []byte) (result []byte) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		log.Debug("The data can't be compressed on shard:", rc.identifier, "Error:", err)
	}
	if err := gz.Flush(); err != nil {
		log.Debug("The data can't be compressed on shard:", rc.identifier, "Error:", err)
	}
	if err := gz.Close(); err != nil {
		log.Debug("The data can't be compressed on shard:", rc.identifier, "Error:", err)
	}

	return b.Bytes()
}

func (rc *Recommender) checkAndExpire() {
	for {
		for rc.totalClassif > rc.maxClassif {
			rc.mutex.Lock()

			rc.totalClassif -= uint64(len(rc.older.scores))
			delete(rc.records, rc.older.recID)
			rc.older = rc.older.next
			rc.older.prev = nil

			rc.mutex.Unlock()
		}

		time.Sleep(time.Millisecond * 300)
	}
}