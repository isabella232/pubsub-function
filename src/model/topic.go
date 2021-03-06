package model

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/kafkaesque-io/pubsub-function/src/icrypto"
)

// Status can be used for webhook status
type Status int

// state machine of webhook state
const (
	// Deactivated is the beginning state
	Deactivated Status = iota
	// Activated is the only active state
	Activated
	// Suspended is the state between Activated and Deleted
	Suspended
	// Deleted is the end of state
	Deleted
)

// WebhookConfig - a configuration for webhook
type WebhookConfig struct {
	URL              string    `json:"url"`
	Headers          []string  `json:"headers"`
	Subscription     string    `json:"subscription"`
	SubscriptionType string    `json:"subscriptionType"`
	InitialPosition  string    `json:"initialPosition"`
	WebhookStatus    Status    `json:"webhookStatus"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	DeletedAt        time.Time `json:"deletedAt"`
}

//TODO add state of Webhook replies

// TopicConfig - a configuraion for topic and its webhook configuration.
type TopicConfig struct {
	TopicFullName string
	PulsarURL     string
	Token         string
	Tenant        string
	Key           string
	Notes         string
	TopicStatus   Status
	Webhooks      []WebhookConfig
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FunctionConfig is the function configuration
type FunctionConfig struct {
	Name             string        `json:"name"`
	ID               string        `json:"id"`
	Tenant           string        `json:"tenant"`
	FunctionStatus   Status        `json:"functionStatus"`
	FunctionFilePath string        `json:"functionFilePath"`
	LanguagePack     string        `json:"languagePack"`
	Parallelism      int           `json:"parallelism"`
	WebhookURLs      []string      `json:"webhookURLs"`
	InputTopic       FunctionTopic `json:"inputTopics"`
	OutputTopic      FunctionTopic `json:"outputTopics"`
	LogTopic         FunctionTopic `json:"logTopic"`
	TriggerType      string        `json:"triggerType"`
	Cron             string        `json:"cron"`
	CreatedAt        time.Time     `json:"createdAt"`
	UpdatedAt        time.Time     `json:"updatedAt"`
	DeletedAt        time.Time     `json:"deletedAt"`
}

// FunctionTopic is the topic configurtion for function
type FunctionTopic struct {
	TopicFullName    string `json:"topicFullName"`
	PulsarURL        string `json:"pulsarURL"`
	Token            string `json:"token"`
	Tenant           string `json:"tenant"`
	Key              string `json:"key"`
	Subscription     string `json:"subscription"`
	SubscriptionType string `json:"subscriptionType"`
	KeySharedPolicy  string `json:"keySharedPolicy"`
	InitialPosition  string `json:"initialPosition"`
}

// TopicKey represents a struct to identify a topic
type TopicKey struct {
	TopicFullName string `json:"TopicFullName"`
	PulsarURL     string `json:"PulsarURL"`
}

//
const (
	NonResumable = "NonResumable"
)

// StringToStatus converts status in string to Status type
func StringToStatus(status string) Status {
	switch strings.ToLower(status) {
	case "activated":
		return Activated
	case "suspended":
		return Suspended
	case "deleted":
		return Deleted
	default:
		return Deactivated
	}
}

// NewTopicConfig creates a topic configuration struct.
func NewTopicConfig(topicFullName, pulsarURL, token string) (TopicConfig, error) {
	cfg := TopicConfig{}
	cfg.TopicFullName = topicFullName
	cfg.PulsarURL = pulsarURL
	cfg.Token = token
	cfg.Webhooks = make([]WebhookConfig, 0, 10) //Good to have a limit to budget threads

	var err error
	cfg.Key, err = GetKeyFromNames(topicFullName, pulsarURL)
	if err != nil {
		return cfg, err
	}
	cfg.CreatedAt = time.Now()
	cfg.UpdatedAt = time.Now()
	return cfg, nil
}

// NewWebhookConfig creates a new webhook config
func NewWebhookConfig(URL string) WebhookConfig {
	cfg := WebhookConfig{}
	cfg.URL = URL
	cfg.Subscription = fmt.Sprintf("%s%s%d", NonResumable, icrypto.GenTopicKey(), time.Now().UnixNano())
	cfg.WebhookStatus = Activated
	cfg.SubscriptionType = "exclusive"
	cfg.InitialPosition = "latest"
	cfg.CreatedAt = time.Now()
	cfg.UpdatedAt = time.Now()
	return cfg
}

// GetKeyFromNames generate topic key based on topic full name and pulsar url
func GetKeyFromNames(tenant, functionName string) (string, error) {
	return GenKey(tenant, functionName), nil
}

// GenKey generates a unique key based on pulsar url and topic full name
func GenKey(tenant, functionName string) string {
	h := sha1.New()
	h.Write([]byte(tenant + functionName))
	return hex.EncodeToString(h.Sum(nil))
}

// GetInitialPosition returns the initial position for subscription
func GetInitialPosition(pos string) (pulsar.SubscriptionInitialPosition, error) {
	switch strings.ToLower(pos) {
	case "latest", "":
		return pulsar.SubscriptionPositionLatest, nil
	case "earliest":
		return pulsar.SubscriptionPositionEarliest, nil
	default:
		return -1, fmt.Errorf("invalid subscription initial position %s", pos)
	}
}

// GetSubscriptionType converts string based subscription type to Pulsar subscription type
func GetSubscriptionType(subType string) (pulsar.SubscriptionType, error) {
	switch strings.ToLower(subType) {
	case "exclusive", "":
		return pulsar.Exclusive, nil
	case "shared":
		return pulsar.Shared, nil
	case "keyshared":
		return pulsar.KeyShared, nil
	case "failover":
		return pulsar.Failover, nil
	default:
		return -1, fmt.Errorf("unsupported subscription type %s", subType)
	}
}

// ValidateWebhookConfig validates WebhookConfig object
// I'd write explicit validation code rather than any off the shelf library,
// which are just DSL and sometime these library just like fit square peg in a round hole.
// Explicit validation has no dependency and very specific.
func ValidateWebhookConfig(whs []WebhookConfig) error {
	// keeps track of exclusive subscription name
	exclusiveSubs := make(map[string]bool)
	for _, wh := range whs {
		if !IsURL(wh.URL) {
			return fmt.Errorf("not a URL %s", wh.URL)
		}
		if strings.TrimSpace(wh.Subscription) == "" {
			return fmt.Errorf("subscription name is missing")
		}
		if subType, err := GetSubscriptionType(wh.SubscriptionType); err == nil {
			if subType == pulsar.Exclusive {
				if exclusiveSubs[wh.Subscription] {
					return fmt.Errorf("exclusive subscription %s cannot be shared between multiple webhooks", wh.Subscription)
				}
				exclusiveSubs[wh.Subscription] = true
			}
		} else {
			return err
		}
		if _, err := GetInitialPosition(wh.InitialPosition); err != nil {
			return err
		}
	}
	return nil

}

// ValidateTopicConfig validates the TopicConfig and returns the key to identify this topic
func ValidateTopicConfig(top TopicConfig) (string, error) {
	if err := ValidateWebhookConfig(top.Webhooks); err != nil {
		return "", err
	}

	return GetKeyFromNames(top.TopicFullName, top.PulsarURL)
}

// IsURL evaluates if this is PulsarURL
func IsURL(str string) bool {
	u, err := url.Parse(str)
	return err == nil && u.Scheme != "" && u.Host != ""
}
