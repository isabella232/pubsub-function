package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/kafkaesque-io/pubsub-function/src/model"
	"github.com/kafkaesque-io/pubsub-function/src/pulsardriver"
	"github.com/kafkaesque-io/pubsub-function/src/util"

	log "github.com/sirupsen/logrus"
)

/**
 * Data design - we use a topic as a database table to store document per user topics basis
 * ll non-acked events are received by a consumer; processed to build an in memory database.
 * Creation creates a single document with user topic access information and webhook url and headers
         , this becomes an event sent by producer
 * Read reads
 * A topic prefix for the webhook configuration database
**/

// the signal to track if the liveness of the reader process
type liveSignal struct{}

// a map of FunctionConfig struct with Key, hash of pulsar URL and topic full name, is the key
// var topics = make(map[string]model.FunctionConfig)

// PulsarHandler is the Pulsar database driver
type PulsarHandler struct {
	PulsarURL   string
	PulsarToken string
	TopicName   string
	topicsLock  sync.RWMutex
	client      pulsar.Client
	producer    pulsar.Producer
	topics      map[string]model.FunctionConfig
	logger      *log.Entry
}

//Init is a Db interface method.
func (s *PulsarHandler) Init() error {
	s.logger = log.WithFields(log.Fields{"app": "pulsardb"})
	s.topics = make(map[string]model.FunctionConfig)

	s.logger.Infof("database pulsar URL: %s", s.PulsarURL)
	if log.GetLevel() == log.DebugLevel {
		s.logger.Debugf("database pulsar token string is %s", s.PulsarToken)
	}

	var err error
	s.client, err = pulsardriver.NewPulsarClient(s.PulsarURL, s.PulsarToken)
	if err != nil {
		// this would be a serious problem so that we return with error
		return err
	}

	err = s.createProducer()
	if err != nil {
		// this would be a serious problem so that we return with error
		log.Errorf("failed to create producer error %v", err)
		return err
	}

	// a loop to receive and recover from failure
	go func() {
		sig := make(chan *liveSignal)
		go s.dbListener(sig)
		for {
			select {
			case <-sig:
				go s.dbListener(sig)
			}
		}
	}()

	return nil
}

//DbListener listens db updates
func (s *PulsarHandler) dbListener(sig chan *liveSignal) error {
	defer func(termination chan *liveSignal) {
		s.logger.Errorf("tenant db listener terminated")
		termination <- &liveSignal{}
	}(sig)
	s.logger.Infof("listens to pulsar wh database changes")
	reader, err := s.client.CreateReader(pulsar.ReaderOptions{
		Topic:          s.TopicName,
		StartMessageID: pulsar.EarliestMessageID(),
		ReadCompacted:  true,
	})

	if err != nil {
		log.Errorf("dbListener failed to create reader, error %v", err)
		return err
	}
	defer reader.Close()

	ctx := context.Background()
	// infinite loop to receive messages
	for {
		data, err := reader.Next(ctx)
		if err != nil {
			log.Errorf("dbListener reader.Next() error %v", err)
			return err
		}
		doc := model.FunctionConfig{}
		if err = json.Unmarshal(data.Payload(), &doc); err != nil {
			s.logger.Errorf("dblistener reader unmarshal error %v", err)
			// ignore error and move on
		} else {
			s.topicsLock.Lock()
			defer s.topicsLock.Unlock()
			if doc.FunctionStatus != model.Deleted {
				s.logger.Infof("add topic configuration %s", doc.ID)
				s.topics[doc.ID] = doc
			} else {
				delete(s.topics, doc.ID)
			}
		}
	}
}

func (s *PulsarHandler) createProducer() error {
	var err error
	s.producer, err = s.client.CreateProducer(pulsar.ProducerOptions{
		Topic:           s.TopicName,
		DisableBatching: true,
	})
	return err
}

//Sync is a Db interface method.
func (s *PulsarHandler) Sync() error {
	return errors.New("Unsupported since this is automatically sync-ed")
}

//Health is a Db interface method
func (s *PulsarHandler) Health() bool {
	return true
}

// Close closes database
func (s *PulsarHandler) Close() error {
	s.producer.Close()
	// s.client.Close()
	// Here is a Client object leak
	return nil
}

//NewPulsarHandler initialize a Pulsar Db
func NewPulsarHandler() (*PulsarHandler, error) {
	handler := PulsarHandler{
		logger: log.WithFields(log.Fields{"app": "pulsardb"}),
	}
	handler.PulsarURL = util.GetConfig().PulsarBrokerURL
	if strings.HasPrefix(util.GetConfig().DbConnectionStr, "pulsar") {
		handler.PulsarURL = util.GetConfig().DbConnectionStr
	}
	handler.TopicName = util.GetConfig().DbName
	handler.PulsarToken = util.GetConfig().DbPassword
	err := handler.Init()
	return &handler, err
}

// Create creates a new document
func (s *PulsarHandler) Create(functionCfg *model.FunctionConfig) (string, error) {
	key, err := getKey(functionCfg)
	if err != nil {
		return key, err
	}

	if _, ok := s.topics[key]; ok {
		return key, errors.New(DocAlreadyExisted)
	}

	functionCfg.ID = key
	functionCfg.CreatedAt = time.Now()
	functionCfg.UpdatedAt = functionCfg.CreatedAt

	return s.updateCacheAndPulsar(functionCfg)
}

func (s *PulsarHandler) updateCacheAndPulsar(functionCfg *model.FunctionConfig) (string, error) {

	ctx := context.Background()
	data, err := json.Marshal(*functionCfg)
	if err != nil {
		return "", err
	}
	msg := pulsar.ProducerMessage{
		Payload: data,
		Key:     functionCfg.ID,
	}

	if _, err = s.producer.Send(ctx, &msg); err != nil {
		return "", err
	}
	// s.producer.Flush() do not use it's a blocking call

	s.logger.Infof("send to Pulsar %s", functionCfg.ID)

	s.topics[functionCfg.ID] = *functionCfg
	return functionCfg.ID, nil
}

// GetByTopic gets a document by the topic name and pulsar URL
func (s *PulsarHandler) GetByTopic(tenant, functionName string) (*model.FunctionConfig, error) {
	key, err := model.GetKeyFromNames(tenant, functionName)
	if err != nil {
		return &model.FunctionConfig{}, err
	}
	return s.GetByKey(key)
}

// GetByKey gets a document by the key
func (s *PulsarHandler) GetByKey(hashedTopicKey string) (*model.FunctionConfig, error) {
	if v, ok := s.topics[hashedTopicKey]; ok {
		return &v, nil
	}
	return &model.FunctionConfig{}, errors.New(DocNotFound)
}

// Load loads the entire database into memory
func (s *PulsarHandler) Load() ([]*model.FunctionConfig, error) {
	results := []*model.FunctionConfig{}
	for _, v := range s.topics {
		results = append(results, &v)
	}
	return results, nil
}

// Update updates or creates a topic config document
func (s *PulsarHandler) Update(functionCfg *model.FunctionConfig) (string, error) {
	key, err := getKey(functionCfg)
	if err != nil {
		return key, err
	}

	if _, ok := s.topics[key]; !ok {
		return s.Create(functionCfg)
	}

	v := s.topics[key]
	v.Tenant = functionCfg.Tenant
	v.FunctionStatus = functionCfg.FunctionStatus
	v.UpdatedAt = time.Now()

	s.logger.Infof("upsert %s", key)
	return s.updateCacheAndPulsar(functionCfg)

}

// Delete deletes a document
func (s *PulsarHandler) Delete(tenant, functionName string) (string, error) {
	key, err := model.GetKeyFromNames(tenant, functionName)
	if err != nil {
		return "", err
	}
	return s.DeleteByKey(key)
}

// DeleteByKey deletes a document based on key
func (s *PulsarHandler) DeleteByKey(hashedTopicKey string) (string, error) {
	if _, ok := s.topics[hashedTopicKey]; !ok {
		return "", errors.New(DocNotFound)
	}

	v := s.topics[hashedTopicKey]
	v.FunctionStatus = model.Deleted

	ctx := context.Background()
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}

	msg := pulsar.ProducerMessage{
		Payload: data,
		Key:     v.ID,
	}

	if _, err = s.producer.Send(ctx, &msg); err != nil {
		return "", err
	}

	delete(s.topics, v.ID)
	return hashedTopicKey, nil
}
