// Copied from: https://github.com/wizzard0/trycloudflared
// Another Option: https://github.com/lucanhost/flared

package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"runtime"
	"time"

	"github.com/cloudflare/cloudflared/client"
	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/orchestration"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/supervisor"
	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/google/uuid"
	"github.com/pkg/errors"
)

type noopMetrics struct{}

func (noopMetrics) IncrementDNSUDPRequests() {}
func (noopMetrics) IncrementDNSTCPRequests() {}

var (
	Version   = "DEV"
	BuildTime = "unknown"
	BuildType = ""
)

type CreateTunnelResponse struct {
	Success bool                `json:"success"`
	Result  TunnelCredentials   `json:"result"`
	Errors  []CreateTunnelError `json:"errors"`
}

type CreateTunnelError struct {
	Code    int
	Message string
}

type TunnelCredentials struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	AccountTag string `json:"account_tag"`
	Secret     []byte `json:"secret"`
}

func createTunnel() (*connection.TunnelProperties, error) {
	// can be slow
	timeout := 30 * time.Second
	client := http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
		},
		Timeout: timeout,
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.trycloudflare.com/tunnel", nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build tunnel request")
	}

	req.Header.Add("Content-Type", "application/json")
	// be nice and tell them where to find us
	req.Header.Add("User-Agent", "cloudflared/embedded-wizzard0-trycloudflared")

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to request Tunnel")
	}
	//goland:noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	response, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read creation response")
	}

	var parsedResponse CreateTunnelResponse
	if err := json.Unmarshal(response, &parsedResponse); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal credentials")
	}

	tunnelID, err := uuid.Parse(parsedResponse.Result.ID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse Tunnel ID")
	}

	return &connection.TunnelProperties{
		Credentials: connection.Credentials{
			AccountTag:   parsedResponse.Result.AccountTag,
			TunnelSecret: parsedResponse.Result.Secret,
			TunnelID:     tunnelID,
		},
		QuickTunnelUrl: parsedResponse.Result.Hostname,
	}, nil
}

func CreateCloudflareTunnel(ctx context.Context, port int) (string, error) {
	// TODO: make this configurable
	logTransport := logger.Create(logger.CreateConfig(
		"",
		false,
		false,
		"",
		"",
	))

	observer := connection.NewObserver(logTransport, logTransport)

	featureSelector, err := features.NewFeatureSelector(ctx, "", nil, false, logTransport)
	if err != nil {
		return "", errors.Wrap(err, "can't create feature selector")
	}

	clientConfig, err := client.NewConfig(Version, runtime.GOOS+"_"+runtime.GOARCH, featureSelector)
	if err != nil {
		return "", errors.Wrap(err, "can't create client config")
	}

	ing, err := ingress.ParseIngress(&config.Configuration{
		Ingress: []config.UnvalidatedIngressRule{
			{
				Service: fmt.Sprintf("http://localhost:%d", port),
			},
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "can't parse ingress")
	}

	warpRouting := ingress.NewWarpRoutingConfig(&config.WarpRoutingConfig{})
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer: ingress.NewDialer(warpRouting),
	}, logTransport)

	dnsService := origins.NewDNSResolverService(origins.NewDNSDialer(), logTransport, noopMetrics{})
	originDialer.AddReservedService(dnsService, []netip.AddrPort{origins.VirtualDNSServiceAddr})

	orchestrator, err := orchestration.NewOrchestrator(
		ctx,
		&orchestration.Config{
			Ingress:             &ing,
			WarpRouting:         warpRouting,
			OriginDialerService: originDialer,
			ConfigurationFlags:  map[string]string{},
		},
		[]pogs.Tag{},
		[]ingress.Rule{},
		logTransport,
	)
	if err != nil {
		return "", errors.Wrap(err, "can't create orchestrator")
	}

	connectedSignal := signal.New(make(chan struct{}))

	protocolSelector, err := connection.NewProtocolSelector(
		connection.HTTP2.String(),
		"random value", // credentials account tag
		false,
		edgediscovery.ProtocolPercentage,
		connection.ResolveTTL,
		logTransport,
	)
	if err != nil {
		return "", errors.Wrap(err, "unable to create protocol selector")
	}

	edgeTLSConfigs := make(map[connection.Protocol]*tls.Config, len(connection.ProtocolList))
	for _, p := range connection.ProtocolList {
		tlsSettings := p.TLSSettings()
		if tlsSettings == nil {
			return "", fmt.Errorf("%s has unknown TLS settings", p)
		}
		edgeTLSConfig, err := tlsconfig.CreateTunnelConfig("", tlsSettings.ServerName)
		if err != nil {
			return "", errors.Wrap(err, "unable to create TLS config to connect with edge")
		}
		if len(tlsSettings.NextProtos) > 0 {
			edgeTLSConfig.NextProtos = tlsSettings.NextProtos
		}
		edgeTLSConfigs[p] = edgeTLSConfig
	}
	tunnel, err := createTunnel()
	if err != nil {
		return "", err
	}

	tunnelConfig := &supervisor.TunnelConfig{
		ClientConfig:                        clientConfig,
		GracePeriod:                         30, // grace-period, default is 30
		EdgeAddrs:                           []string{},
		Region:                              "",
		EdgeIPVersion:                       allregions.Auto, // Default is ipv4
		EdgeBindAddr:                        nil,             // default is to let cf handle it
		HAConnections:                       2,               // 4 is default
		IsAutoupdated:                       false,
		LBPool:                              "",
		Tags:                                []pogs.Tag{},
		Log:                                 logTransport,
		LogTransport:                        logTransport,
		Observer:                            observer,
		ReportedVersion:                     "embedded-go-test",
		Retries:                             5,    // retries, default is 5
		RunFromTerminal:                     true, // TODO: false
		NamedTunnel:                         tunnel,
		ProtocolSelector:                    protocolSelector,
		EdgeTLSConfigs:                      edgeTLSConfigs,
		MaxEdgeAddrRetries:                  8,               // max-edge-addr-retries, default is 8
		RPCTimeout:                          5 * time.Second, // rpc-timeout, default is 5s
		WriteStreamTimeout:                  time.Second * 0,
		DisableQUICPathMTUDiscovery:         false,
		QUICConnectionLevelFlowControlLimit: 30 * (1 << 20), // default is 30MB
		QUICStreamLevelFlowControlLimit:     6 * (1 << 20),  // default is 6MB
		ICMPRouterServer:                    nil,
		OriginDNSService:                    dnsService,
		OriginDialerService:                 originDialer,
	}

	shutdown := make(chan struct{}) // eat this

	go func() {
		// TODO: might do errgroup here
		startErr := supervisor.StartTunnelDaemon(ctx, tunnelConfig, orchestrator, connectedSignal, shutdown)
		if startErr != nil {
			// TODO: expose more graceful error reporter
			panic(errors.Wrap(startErr, "failed to start tunnel daemon"))
		}
	}()
	return "https://" + tunnel.QuickTunnelUrl, nil
}
