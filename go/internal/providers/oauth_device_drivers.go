package providers

import (
	"context"
	"strings"
	"time"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// copilotDeviceDriver runs GitHub's device authorization for Copilot on the
// runtime's shared client, which serializes concurrent polls of one code.
type copilotDeviceDriver struct{ copilot *copilotauth.Client }

var _ oauthflow.DeviceDriver = copilotDeviceDriver{}

// Start implements oauthflow.Driver.
func (d copilotDeviceDriver) Start(context.Context, oauthflow.StartRequest) (oauthflow.Authorization, error) {
	device, err := d.copilot.StartDeviceFlow()
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	if strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		return oauthflow.Authorization{}, errIncompleteDeviceStart
	}
	return oauthflow.Authorization{
		UserCode: device.UserCode, VerificationURI: device.VerificationURI,
		Interval: seconds(device.Interval), ExpiresIn: seconds(device.ExpiresIn),
		Secrets: oauthflow.Secrets{DeviceCode: device.DeviceCode},
	}, nil
}

// Poll implements oauthflow.DeviceDriver. GitHub's token names no expiry,
// which the gateway has always stored as 0.
func (d copilotDeviceDriver) Poll(ctx context.Context, flow oauthflow.Flow) (oauthflow.PollResult, error) {
	result := d.copilot.PollDeviceFlowTokenOnce(flow.Secrets.DeviceCode)
	return devicePollResult(ctx, result.Status, result.Error, tokenstore.Record{
		AccessToken: result.AccessToken, Expiry: time.Unix(0, 0),
	})
}

// codexDeviceDriver runs Codex's device authorization. The owner's approval
// belongs to the client that started the flow, not to the one configured
// when it is polled, so the driver keeps that client with the user code.
type codexDeviceDriver struct{}

var _ oauthflow.DeviceDriver = codexDeviceDriver{}

// Start implements oauthflow.Driver with the configured client.
func (codexDeviceDriver) Start(ctx context.Context, _ oauthflow.StartRequest) (oauthflow.Authorization, error) {
	clientID := EffectiveCodexClientID()
	flow, err := codexOAuth(clientID).StartDeviceFlow(ctx)
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	if strings.TrimSpace(flow.DeviceAuthID) == "" || strings.TrimSpace(flow.UserCode) == "" {
		return oauthflow.Authorization{}, errIncompleteDeviceStart
	}
	return oauthflow.Authorization{
		UserCode: flow.UserCode, VerificationURI: flow.VerificationURI,
		Interval: seconds(flow.Interval), ExpiresIn: seconds(flow.ExpiresIn),
		Secrets: oauthflow.Secrets{DeviceCode: flow.DeviceAuthID, DriverData: map[string]string{
			"user_code": flow.UserCode, "client_id": clientID,
		}},
	}, nil
}

// Poll implements oauthflow.DeviceDriver: one poll and, once the owner
// approved, the exchange of the authorization code it hands over.
func (codexDeviceDriver) Poll(ctx context.Context, flow oauthflow.Flow) (oauthflow.PollResult, error) {
	userCode, clientID := flow.Secrets.DriverData["user_code"], flow.Secrets.DriverData["client_id"]
	if userCode == "" || strings.TrimSpace(clientID) == "" {
		return devicePollResult(ctx, "error", "Codex device authorization state is unavailable. Start again.", tokenstore.Record{})
	}
	status, tokens, err := codexOAuth(clientID).PollAndExchange(ctx, codexauth.DeviceFlow{
		DeviceAuthID: flow.Secrets.DeviceCode, UserCode: userCode, ClientID: clientID,
	})
	if err != nil {
		return devicePollResult(ctx, status, err.Error(), tokenstore.Record{})
	}
	return devicePollResult(ctx, status, "", tokenstore.Record{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken,
		TokenType: tokens.TokenType, Expiry: time.Unix(tokens.ExpiresAt, 0), AccountID: tokens.AccountID,
		Metadata: map[string]string{
			OAuthMetadataAccountLabel: tokens.AccountLabel,
			OAuthMetadataProfile:      codexOAuthProfileDevice, OAuthMetadataClientID: clientID,
		},
	})
}
