package sending

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

type sesHTTPFunc func(*http.Request) (*http.Response, error)

func (f sesHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestSESSendDoesNotRetryAmbiguousFailure(t *testing.T) {
	calls := 0
	s := &SES{client: sesv2.NewFromConfig(aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient: sesHTTPFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"message":"ambiguous failure"}`)),
			}, nil
		}),
	})}
	_, err := s.Send(context.Background(), []byte("Subject: test\r\n\r\nHello"), nil)
	var rejected *Rejected
	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("ambiguous server failure must remain unknown: %v", err)
	}
	if calls != 1 {
		t.Fatalf("SES made %d send attempts, want exactly one", calls)
	}
}
