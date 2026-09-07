package bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"hoorific/internal/core"
)

type credentialLease interface {
	AWSCredentials() aws.CredentialsProvider
	AWSRegion() string
}

// ExecuteDuplex builds a Bedrock SDK client from an explicitly supplied AWS
func (c *Connector) ExecuteDuplex(ctx context.Context, conn core.Connection, modelID string, lease core.CredentialLease, client *http.Client, input <-chan []byte, output func([]byte) error) error {
	l, ok := lease.(credentialLease)
	if !ok || l.AWSCredentials() == nil {
		return errors.New("bedrock duplex requires an explicit AWS credential lease")
	}
	region := conn.Region
	if region == "" {
		region = l.AWSRegion()
	}
	if region == "" {
		return errors.New("bedrock duplex region is required")
	}
	if client == nil {
		return errors.New("bedrock duplex requires the gateway HTTP client")
	}
	cfg := aws.Config{Credentials: l.AWSCredentials(), Region: region, HTTPClient: client}
	if conn.BaseURL != "" {
		cfg.BaseEndpoint = aws.String(conn.BaseURL)
	}
	sdk := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		o.Retryer = retry.NewStandard(func(ro *retry.StandardOptions) { ro.MaxAttempts = 1 })
	})
	return Duplex(ctx, sdk, modelID, input, output)
}

// Duplex opens Bedrock's native Smithy EventStream bidirectional operation.
// Input closure half-closes the event writer; it does not synthesize an SSE
// terminal event or turn the connection into a replayable HTTP request.
func Duplex(ctx context.Context, client *bedrockruntime.Client, modelID string, input <-chan []byte, output func([]byte) error) error {
	if client == nil {
		return errors.New("bedrock client is nil")
	}
	if modelID == "" {
		return errors.New("bedrock model id is required")
	}
	if output == nil {
		return errors.New("bedrock output callback is nil")
	}
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	result, err := client.InvokeModelWithBidirectionalStream(runCtx, &bedrockruntime.InvokeModelWithBidirectionalStreamInput{ModelId: aws.String(modelID)}, func(o *bedrockruntime.Options) {
		o.Retryer = retry.NewStandard(func(ro *retry.StandardOptions) { ro.MaxAttempts = 1 })
	})
	if err != nil {
		return fmt.Errorf("bedrock bidirectional stream: %w", err)
	}
	stream := result.GetStream()
	if stream == nil {
		return errors.New("bedrock bidirectional stream missing event stream")
	}
	defer stream.Close()
	select {
	case <-runCtx.Done():
		return runCtx.Err()
	case _, ok := <-result.GetInitialReply():
		if !ok {
			return errors.New("bedrock bidirectional stream closed before initial reply")
		}
	}
	done := make(chan error, 1)
	var once sync.Once
	finish := func(e error) { once.Do(func() { done <- e; cancel() }) }
	go func() {
		for {
			select {
			case <-runCtx.Done():
				if ctx.Err() != nil {
					finish(ctx.Err())
				}
				return
			case b, ok := <-input:
				if !ok {
					if e := stream.Writer.Close(); e != nil {
						finish(e)
					}
					return
				}
				if len(b) == 0 {
					continue
				}
				if len(b) > 64<<10 {
					finish(errors.New("bedrock duplex input chunk exceeds 64 KiB"))
					return
				}
				if e := stream.Send(runCtx, &types.InvokeModelWithBidirectionalStreamInputMemberChunk{Value: types.BidirectionalInputPayloadPart{Bytes: append([]byte(nil), b...)}}); e != nil {
					finish(e)
					return
				}
			}
		}
	}()
	go func() {
		for {
			select {
			case <-runCtx.Done():
				if ctx.Err() != nil {
					finish(ctx.Err())
				}
				return
			case ev, ok := <-stream.Events():
				if !ok {
					if e := stream.Err(); e != nil {
						finish(e)
					} else {
						finish(nil)
					}
					return
				}
				if chunk, ok := ev.(*types.InvokeModelWithBidirectionalStreamOutputMemberChunk); ok {
					if len(chunk.Value.Bytes) > 0 {
						if e := output(chunk.Value.Bytes); e != nil {
							finish(e)
							return
						}
					}
				} else {
					finish(errors.New("bedrock duplex received unknown event"))
					return
				}
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-runCtx.Done():
		return runCtx.Err()
	}
}
