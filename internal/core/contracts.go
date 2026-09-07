// Package core defines gateway value types and extension contracts.
// It depends only on the Go standard library.
package core

import (
	"context"
	"io"
	"net/http"
	"time"
)

type Protocol string
type Operation string
type Framing string
type Support string

const (
	Supported   Support = "supported"
	Unsupported Support = "unsupported"
	Unknown     Support = "unknown"
)

type CodecKey struct {
	Protocol  Protocol
	Variant   string
	Operation Operation
}
type ConnectorDescriptor struct {
	ID           string
	Protocols    []Protocol
	Operations   []Operation
	Subscription bool
}
type Connection struct {
	TenantID, ID, Connector, AccountID, BaseURL, Region, Project string
	Version                                                      int64
	Dedicated                                                    bool
	Settings                                                     map[string]string
}
type Model struct {
	ID, CatalogID, ConnectionID       string
	Operations                        []Operation
	InputModalities, OutputModalities []string
	Features                          map[string]Support
	ContextLimit, OutputLimit         *int64
	Provenance                        string
	Version                           int64
	Price                             *PriceSchedule
}

// PriceSchedule uses integer USD nanodollars (1 USD = 1,000,000,000).
// InputPerMillion and OutputPerMillion are nanodollars per million tokens;
// MaximumUnitCost is nanodollars per UnitOperation. Nil costs are unknown,
// never zero. Token charges round up to the next nanodollar.
type PriceSchedule struct {
	Version          string    `json:"version"`
	InputPerMillion  *int64    `json:"input_per_million,omitempty"`
	OutputPerMillion *int64    `json:"output_per_million,omitempty"`
	MaximumUnitCost  *int64    `json:"maximum_unit_cost,omitempty"`
	UnitOperation    Operation `json:"unit_operation,omitempty"`
}
type Target interface{ target() }
type ModelCall struct {
	Connection Connection
	Model      Model
	Alias      string
}

func (ModelCall) target() {}

type ConnectionResourceCall struct {
	Connection         Connection
	Action, ResourceID string
}

func (ConnectionResourceCall) target() {}

type Binding struct {
	Codec                             CodecKey
	Endpoint, Method, ModelLocation   string
	ModelField                        string
	Framing                           Framing
	ReplaySafe, CancellationSupported bool
	RequiredFeatures                  []string
	Headers                           http.Header
	AllowedRequestHeaders             []string
	Response                          NativeResponsePolicy
	Realtime                          *RealtimePolicy
}
type NativeResponsePolicy struct {
	IDField, StatusField, PollEndpoint, PollMethod, PollAction, UsageField                          string
	PollOperation                                                                                   Operation
	Async                                                                                           bool
	ContinuationHeaders, ContinuationFields, ContinuationOrigins, TerminalStatuses, FailureStatuses []string
	ContinuationMethods, ContinuationActions                                                        map[string]string
}
type RealtimePolicy struct {
	Protocol                                                                string
	Subprotocols                                                            []string
	MaxResponses, MaxSessionSeconds                                         int
	WholeSessionBound, AllowBinary, DirectWebRTC, RequirePayloadEnforcement bool
}
type Connector interface {
	Descriptor() ConnectorDescriptor
	Bind(context.Context, Target, Operation) (Binding, error)
}

// ConnectionDescriptor reports capabilities of a configured connector instance,
// rather than the factory's default protocol or API mode.
type ConnectionDescriptor interface {
	DescriptorFor(Connection) (ConnectorDescriptor, error)
}

type APIKeyPolicy struct{ Header, Prefix string }
type APIKeyPolicyProvider interface {
	APIKeyPolicyFor(Connection) (APIKeyPolicy, error)
}
type Discoverer interface {
	Discover(context.Context, Connection) ([]Model, error)
}

// CredentialLease is an internal authorization handle; never persist or serialize it.
type CredentialLease interface {
	Authorize(context.Context, *http.Request) error
}
type Authenticator interface {
	Authorize(context.Context, CredentialLease, *http.Request) error
}
type Body interface {
	Open(context.Context) (io.ReadCloser, error)
	Replayable() bool
	Close() error
}
type RequestPayload interface{ requestPayload() }
type ResultPayload interface{ resultPayload() }
type RequestCodec interface {
	DecodeRequest(context.Context, io.Reader) (RequestPayload, error)
	EncodeRequest(context.Context, RequestPayload, io.Writer) error
}
type ResultCodec interface {
	DecodeResult(context.Context, io.Reader) (ResultPayload, error)
	EncodeResult(context.Context, ResultPayload, io.Writer) error
}
type StreamCodec interface {
	NewDecoder(io.Reader) (EventDecoder, error)
	NewEncoder(io.Writer) (EventEncoder, error)
}
type EventDecoder interface {
	Next(context.Context) (Event, error)
}
type EventEncoder interface {
	Write(context.Context, Event) error
}
type Event interface{ event() }
type Index struct{ Choice, Block, Tool int }
type Start struct{ ID, Model string }

func (Start) event() {}

type BlockStart struct {
	Index          Index
	Kind, ID, Name string
}

func (BlockStart) event() {}

type TextDelta struct {
	Index Index
	Text  string
}

func (TextDelta) event() {}

type ToolCallStart struct {
	Index    Index
	ID, Name string
}

func (ToolCallStart) event() {}

type ToolArgumentsDelta struct {
	Index    Index
	Fragment string
}

func (ToolArgumentsDelta) event() {}

type BlockEnd struct{ Index Index }

func (BlockEnd) event() {}

type Usage struct {
	Input, Output, Total *int64
	Source               string
}

func (Usage) event() {}

type Finish struct{ Status, Reason, Cancellation string }

func (Finish) event() {}

type StreamError struct{ Error GatewayError }

func (StreamError) event() {}

type GatewayError struct {
	Code              string `json:"code"`
	HTTPStatus        int    `json:"-"`
	Message           string `json:"message"`
	Param             string `json:"param,omitempty"`
	Retryable         bool   `json:"-"`
	Origin            string `json:"-"`
	ProviderRequestID string `json:"-"`
}

func (e GatewayError) Error() string { return e.Message }

type ContentBlock struct {
	Kind, Text, URL, MIMEType, ID, Name, Arguments string
	Data                                           []byte
}
type Message struct {
	Role    string
	Content []ContentBlock
}
type Tool struct {
	Name, Description string
	Schema            []byte
}
type Conversation struct {
	Model                                                                   string
	System                                                                  []ContentBlock
	Messages                                                                []Message
	Tools                                                                   []Tool
	MaxOutputTokens                                                         *int64
	Stop                                                                    []string
	Stream                                                                  bool
	StreamIncludeUsage                                                      *bool
	StructuredOutput                                                        []byte
	StructuredOutputName, StructuredOutputDescription, StructuredOutputMode string
	StructuredOutputStrict                                                  *bool
}

func (Conversation) requestPayload() {}

type GenerationResult struct {
	ID, Model string
	Blocks    []ContentBlock
	Finish    Finish
	Usage     *Usage
}

func (GenerationResult) resultPayload() {}

type CompletionRequest struct {
	Model, Prompt   string
	MaxOutputTokens *int64
	Stream          bool
}

func (CompletionRequest) requestPayload() {}

type CompletionResult struct {
	ID, Model, Text string
	Finish          Finish
	Usage           *Usage
}

func (CompletionResult) resultPayload() {}

type EmbeddingRequest struct {
	Model      string
	Inputs     []string
	Dimensions *int
	Encoding   string
}

func (EmbeddingRequest) requestPayload() {}

type Embedding struct {
	Index   int
	Vector  []float64
	Encoded string
}
type EmbeddingResult struct {
	Model, Encoding string
	Dimensions      int
	Embeddings      []Embedding
	Usage           *Usage
}

func (EmbeddingResult) resultPayload() {}

type RerankRequest struct {
	Model, Query string
	Documents    []string
	TopN         *int
}

func (RerankRequest) requestPayload() {}

type RankedDocument struct {
	Index int
	Score float64
}
type RerankResult struct {
	Results []RankedDocument
	Usage   *Usage
}

func (RerankResult) resultPayload() {}

type CountTokensRequest struct{ Conversation Conversation }

func (CountTokensRequest) requestPayload() {}

type CountTokensResult struct {
	InputTokens *int64
	Source      string
}

func (CountTokensResult) resultPayload() {}

// Allowance Maximum and Reserve use units selected by Kind; cost allowances
// use USD nanodollars, while token and request allowances remain counts.
type Allowance struct {
	ScopeKind, ScopeID, WindowID, Kind string
	Maximum, Reserve                   int64
}

// AttemptPlan.MaximumCost is a conservative bound in USD nanodollars.
type AttemptPlan struct {
	TenantID, RequestID, AttemptID, KeyID, ConnectionID, AccountID, ModelID, PriceVersion string
	KeyRevision, ConfigRevision                                                           int64
	MaximumCost                                                                           *int64
	Deadline                                                                              time.Time
	Allowances                                                                            []Allowance
	Operation                                                                             Operation
	Alias                                                                                 string
	InputBound, OutputBound, UnitCount                                                    *int64
}
type AttemptPermit struct {
	TenantID, RequestID, AttemptID string
	ConfigRevision                 int64
}

// AttemptOutcome.ActualCost is USD nanodollars; nil means unknown.
type AttemptOutcome struct {
	TenantID, RequestID, AttemptID, State, Cancellation string
	Usage                                               *Usage
	ActualCost                                          *int64
}
type AdmissionStore interface {
	BeginAttempt(context.Context, AttemptPlan) (AttemptPermit, error)
	MarkAccepted(context.Context, AttemptPermit, string) error
	FinalizeAttempt(context.Context, AttemptOutcome) error
}
