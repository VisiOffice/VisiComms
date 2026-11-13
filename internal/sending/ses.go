package sending

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	"strings"
)

// SES is the platform's Amazon SES account.
type SES struct {
	client           *sesv2.Client
	configurationSet string
}

func (*SES) Kind() string { return "ses" }

func NewSES(ctx context.Context, region, keyID, secret, configurationSet string) (*SES, error) {
	if keyID == "" || secret == "" {
		return nil, errors.New("AWS_SES_ACCESS_KEY_ID and AWS_SES_SECRET_ACCESS_KEY are required to send")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(keyID, secret, "")))
	if err != nil {
		return nil, err
	}
	return &SES{client: sesv2.NewFromConfig(cfg), configurationSet: configurationSet}, nil
}

func (s *SES) RegisterDomain(ctx context.Context, domain string) (Identity, error) {
	_, err := s.client.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity:         aws.String(domain),
		DkimSigningAttributes: &types.DkimSigningAttributes{NextSigningKeyLength: types.DkimSigningKeyLengthRsa2048Bit},
		Tags:                  []types.Tag{{Key: aws.String("product"), Value: aws.String("visicomms")}},
	})
	var exists *types.AlreadyExistsException
	if err != nil && !errors.As(err, &exists) {
		return Identity{}, fmt.Errorf("registering %s with SES: %w", domain, err)
	}
	return s.DomainStatus(ctx, domain)
}

func (s *SES) DomainStatus(ctx context.Context, domain string) (Identity, error) {
	out, err := s.client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(domain)})
	if err != nil {
		return Identity{}, fmt.Errorf("checking %s with SES: %w", domain, err)
	}
	id := Identity{Status: "NOT_STARTED"}
	if out.DkimAttributes != nil {
		id.Status = string(out.DkimAttributes.Status)
		id.DKIMTokens = out.DkimAttributes.Tokens
	}
	return id, nil
}

func (s *SES) Send(ctx context.Context, raw []byte, tags map[string]string) (string, error) {
	in := &sesv2.SendEmailInput{Content: &types.EmailContent{Raw: &types.RawMessage{Data: raw}}}
	if s.configurationSet != "" {
		in.ConfigurationSetName = aws.String(s.configurationSet)
	}
	for k, v := range tags {
		in.EmailTags = append(in.EmailTags, types.MessageTag{Name: aws.String(k), Value: aws.String(v)})
	}
	// SES has no send idempotency token. A transport error may follow an
	// accepted message, so SDK retries could send it twice even though River
	// only runs the action once. Domain-management calls may still retry.
	out, err := s.client.SendEmail(ctx, in, func(o *sesv2.Options) {
		o.RetryMaxAttempts = 1
	})
	if err != nil {
		return "", classify(err)
	}
	return aws.ToString(out.MessageId), nil
}

// classify separates "SES said no" (definitely not sent) from everything
// else: a timeout or a dropped connection may have been accepted, so it is
// reported as unknown and never retried blindly.
func classify(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "MessageRejected", "MailFromDomainNotVerifiedException", "BadRequestException",
			"AccountSuspendedException", "SendingPausedException", "NotFoundException",
			"TooManyRequestsException", "LimitExceededException", "ConfigurationSetDoesNotExistException":
			return &Rejected{Reason: strings.TrimSpace(apiErr.ErrorCode() + ": " + apiErr.ErrorMessage())}
		}
	}
	return err
}
