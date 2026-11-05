package service

import (
	"context"
	"errors"
	"testing"

	activity_api "github.com/fleetdm/fleet/v4/server/activity/api"
	"github.com/fleetdm/fleet/v4/server/contexts/viewer"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/mock"
	"github.com/fleetdm/fleet/v4/server/service/conditional_access_google"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

type fakeGoogleCARelay struct {
	registers   []string // "customerID serverURL"
	deregisters []string // "customerID serverURL"
}

func (f *fakeGoogleCARelay) Register(_ context.Context, serverURL, customerID string) (*conditional_access_google.Registration, error) {
	f.registers = append(f.registers, customerID+" "+serverURL)
	return &conditional_access_google.Registration{FleetServerSecret: "secret-" + customerID}, nil
}

func (f *fakeGoogleCARelay) Deregister(_ context.Context, serverURL, customerID, _ string) error {
	f.deregisters = append(f.deregisters, customerID+" "+serverURL)
	return nil
}

type fakeGoogleCACloudIdentity struct {
	unmanaged *[]string
}

func (f fakeGoogleCACloudIdentity) CheckDelegation(context.Context) error { return nil }

func (f fakeGoogleCACloudIdentity) SetUnmanaged(_ context.Context, name string) error {
	*f.unmanaged = append(*f.unmanaged, name)
	return nil
}

type googleCAAppConfigTest struct {
	svc       fleet.Service
	ctx       context.Context
	ds        *mock.Store
	relay     *fakeGoogleCARelay
	unmanaged []string
	rows      []fleet.ConditionalAccessGoogleIntegration // saved rows, in order
	saveErr   error                                      // returned by SaveAppConfig
}

// newGoogleCAAppConfigTest sets up a Premium service whose integration is connected to
// C02cajzvg from https://old.example.com, with one marked device.
func newGoogleCAAppConfigTest(t *testing.T) *googleCAAppConfigTest {
	g := &googleCAAppConfigTest{ds: new(mock.Store), relay: &fakeGoogleCARelay{}}
	ds := g.ds

	dsAppConfig := &fleet.AppConfig{
		OrgInfo:        fleet.OrgInfo{OrgName: "Test"},
		ServerSettings: fleet.ServerSettings{ServerURL: "https://old.example.com"},
	}
	dsAppConfig.Integrations.GoogleConditionalAccess = &fleet.GoogleConditionalAccessIntegration{
		Enabled: true, CustomerID: "C02cajzvg", AdminEmail: "admin@example.com",
	}
	ds.AppConfigFunc = func(ctx context.Context) (*fleet.AppConfig, error) { return dsAppConfig.Copy(), nil }
	ds.SaveAppConfigFunc = func(ctx context.Context, conf *fleet.AppConfig) error {
		if g.saveErr != nil {
			return g.saveErr
		}
		*dsAppConfig = *conf
		return nil
	}
	ds.ListVPPTokensFunc = func(ctx context.Context) ([]*fleet.VPPTokenDB, error) { return []*fleet.VPPTokenDB{}, nil }
	ds.ListABMTokensFunc = func(ctx context.Context) ([]*fleet.ABMToken, error) { return []*fleet.ABMToken{}, nil }
	// A server URL change queues a DEP profile sync.
	ds.NewJobFunc = func(ctx context.Context, job *fleet.Job) (*fleet.Job, error) { return job, nil }

	ds.GetGoogleConditionalAccessIntegrationFunc = func(ctx context.Context) (*fleet.ConditionalAccessGoogleIntegration, error) {
		return &fleet.ConditionalAccessGoogleIntegration{
			CustomerID: "C02cajzvg", AdminEmail: "admin@example.com", ProxyServerSecret: "secret-" + "C02cajzvg",
			State: fleet.ConditionalAccessGoogleStateConnected,
		}, nil
	}
	ds.SaveGoogleConditionalAccessIntegrationFunc = func(ctx context.Context, row *fleet.ConditionalAccessGoogleIntegration) error {
		g.rows = append(g.rows, *row)
		return nil
	}
	ds.GetAllMDMConfigAssetsByNameFunc = func(ctx context.Context, names []fleet.MDMAssetName, _ sqlx.QueryerContext) (map[fleet.MDMAssetName]fleet.MDMConfigAsset, error) {
		return map[fleet.MDMAssetName]fleet.MDMConfigAsset{
			fleet.MDMAssetGoogleConditionalAccessServiceAccountKey: {Value: []byte(`{"client_email":"sa@example.com"}`)},
		}, nil
	}
	ds.ListGoogleConditionalAccessDeviceUserNamesFunc = func(ctx context.Context) ([]string, error) {
		return []string{"devices/d1/deviceUsers/u1"}, nil
	}
	ds.DeleteGoogleConditionalAccessDevicesFunc = func(ctx context.Context) error { return nil }
	ds.DeleteGoogleConditionalAccessIntegrationFunc = func(ctx context.Context) error { return nil }
	ds.DeleteMDMConfigAssetsByNameFunc = func(ctx context.Context, names []fleet.MDMAssetName) error { return nil }

	opts := &TestServerOpts{
		License: &fleet.LicenseInfo{Tier: fleet.TierPremium},
		ConditionalAccessGoogleClient: &GoogleConditionalAccessClient{
			Relay: g.relay,
			CloudIdentity: func(context.Context, conditional_access_google.Credentials) (conditional_access_google.CloudIdentity, error) {
				return fakeGoogleCACloudIdentity{unmanaged: &g.unmanaged}, nil
			},
		},
	}
	svc, ctx := newTestService(t, ds, nil, nil, opts)
	opts.ActivityMock.NewActivityFunc = func(context.Context, *activity_api.User, activity_api.ActivityDetails) error { return nil }
	g.svc = svc
	g.ctx = viewer.NewContext(ctx, viewer.Viewer{User: &fleet.User{GlobalRole: new(fleet.RoleAdmin)}})
	return g
}

func TestModifyAppConfigGoogleConditionalAccessUnrelatedUpdate(t *testing.T) {
	g := newGoogleCAAppConfigTest(t)

	_, err := g.svc.ModifyAppConfig(g.ctx, []byte(`{"org_info": {"org_name": "Test2"}}`), fleet.ApplySpecOptions{})
	require.NoError(t, err)
	require.False(t, g.ds.GetGoogleConditionalAccessIntegrationFuncInvoked)
	require.False(t, g.ds.GetAllMDMConfigAssetsByNameFuncInvoked)
}

func TestModifyAppConfigGoogleConditionalAccessRemoval(t *testing.T) {
	const disable = `{"integrations": {"google_conditional_access": {"enabled": false}}}`

	t.Run("failed config save keeps what reconnecting needs", func(t *testing.T) {
		g := newGoogleCAAppConfigTest(t)
		g.saveErr = errors.New("save failed")

		_, err := g.svc.ModifyAppConfig(g.ctx, []byte(disable), fleet.ApplySpecOptions{})
		require.ErrorContains(t, err, "save failed")
		require.Equal(t, []string{"C02cajzvg https://old.example.com"}, g.relay.deregisters)
		require.Equal(t, []string{"devices/d1/deviceUsers/u1"}, g.unmanaged)
		// The key and a row for the configured customer stay, so resending the settings
		// registers again without a new key upload.
		require.False(t, g.ds.DeleteMDMConfigAssetsByNameFuncInvoked)
		require.False(t, g.ds.DeleteGoogleConditionalAccessIntegrationFuncInvoked)
		require.NotEmpty(t, g.rows)
		last := g.rows[len(g.rows)-1]
		require.Equal(t, "C02cajzvg", last.CustomerID)
		require.Empty(t, last.ProxyServerSecret)
		require.NotEqual(t, fleet.ConditionalAccessGoogleStateConnected, last.State)
	})

	t.Run("removal uses the server URL it was registered with", func(t *testing.T) {
		g := newGoogleCAAppConfigTest(t)

		_, err := g.svc.ModifyAppConfig(g.ctx, []byte(`{
			"server_settings": {"server_url": "https://new.example.com"},
			"integrations": {"google_conditional_access": {"enabled": false}}
		}`), fleet.ApplySpecOptions{})
		require.NoError(t, err)
		require.Equal(t, []string{"C02cajzvg https://old.example.com"}, g.relay.deregisters)
		require.True(t, g.ds.DeleteMDMConfigAssetsByNameFuncInvoked)
		require.True(t, g.ds.DeleteGoogleConditionalAccessIntegrationFuncInvoked)
	})

	t.Run("customer change leaves with the old URL and registers with the new one", func(t *testing.T) {
		g := newGoogleCAAppConfigTest(t)

		_, err := g.svc.ModifyAppConfig(g.ctx, []byte(`{
			"server_settings": {"server_url": "https://new.example.com"},
			"integrations": {"google_conditional_access": {"customer_id": "C03other"}}
		}`), fleet.ApplySpecOptions{})
		require.NoError(t, err)
		require.Equal(t, []string{"C02cajzvg https://old.example.com"}, g.relay.deregisters)
		require.Equal(t, []string{"C03other https://new.example.com"}, g.relay.registers)
	})
}
