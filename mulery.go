// Package mulery provides an application wrapper around the mulery/server module.
// This is full of assumptions, but very configurable.
// Use it as-is, or use it as an example for your own server package.
package mulery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/caddyserver/certmagic"
	apachelog "github.com/lestrrat-go/apache-logformat/v2"
	"github.com/libdns/cloudflare"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golift.io/cnfgfile"
	"golift.io/mulery/mulch"
	"golift.io/mulery/server"
)

// Config is the input data to run this app. Read from a config file.
type Config struct {
	ListenAddr string `json:"listenAddr" toml:"listen_addr" yaml:"listenAddr" xml:"listen_addr"`
	AuthURL    string `json:"authUrl" toml:"auth_url" yaml:"authUrl" xml:"auth_url"`
	AuthHeader string `json:"authHeader" toml:"auth_header" yaml:"authHeader" xml:"auth_header"`
	// Providing a header=>name map here will put these request headers into the apache log output.
	LogHeaders map[string]string `json:"logHeaders" toml:"log_headers" yaml:"logHeaders" xml:"log_headers"`
	// List of IPs or CIDRs that are allowed to make requests to clients.
	Upstreams []string `json:"upstreams" toml:"upstreams" yaml:"upstreams" xml:"upstreams"`
	// Optional directory where SSL certificates are stored.
	CacheDir string `json:"cacheDir" toml:"cache_dir" yaml:"cacheDir" xml:"cache_dir"`
	// CFToken is used to create DNS entries to validate SSL certs for acme.
	CFToken string `json:"cfToken" toml:"cf_token"  yaml:"cfToken" xml:"cf_token"`
	// Email is used for acme certificate registration.
	Email string `json:"email" toml:"email" yaml:"email" xml:"email"`
	// DNS Names that we are allowed to create SSL certificates for.
	SSLNames StringSlice `json:"sslNames" toml:"ssl_names" yaml:"sslNames" xml:"ssl_names"`
	// Path to app log file.
	LogFile string `json:"logFile" toml:"log_file" yaml:"logFile" xml:"log_file"`
	// Number of log files to keep when rotating.
	LogFiles int `json:"logFiles" toml:"log_files" yaml:"logFiles" xml:"log_files"`
	// Rotate the log file when it reaches this many megabytes.
	LogFileMB int64 `json:"logFileMb" toml:"log_file_mb" yaml:"logFileMb" xml:"log_file_mb"`
	// Path for http log.
	HTTPLog string `json:"httpLog" toml:"http_log" yaml:"httpLog" xml:"http_log"`
	// Number of http log files to keep when rotating.
	HTTPLogs int `json:"httpLogs" toml:"http_logs" yaml:"httpLogs" xml:"http_logs"`
	// Rotate the http log file when it reaches this many megabytes.
	HTTPLogMB int64 `json:"httpLogMb" toml:"http_log_mb" yaml:"httpLogMb" xml:"http_log_mb"`
	// RedirectURL is where to send a request to any unknown path. Unauthorized is returned otherwise.
	RedirectURL string `json:"redirectUrl" toml:"redirect_url" yaml:"redirectUrl" xml:"redirect_url"`
	*server.Config
	dispatch *server.Server
	client   *http.Client
	server   *http.Server
	allow    *AllowedIPs
	log      *log.Logger
	httpLog  *log.Logger
}

type StringSlice []string

func (s StringSlice) Contains(str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}

	return false
}

var ErrInvalidKey = errors.New("provided key is not authorized")

const keyLen = 36

// LoadConfigFile does what its name implies.
func LoadConfigFile(path string) (*Config, error) {
	config := &Config{
		Config: server.NewConfig(),
		client: &http.Client{},
	}
	config.Config.KeyValidator = config.KeyValidator
	config.Logger = config

	if err := cnfgfile.Unmarshal(config, path); err != nil {
		return nil, fmt.Errorf("failed to parse configuration: %w", err)
	}

	// We put this here, so we can print the parsed IPs on startup.
	config.allow = MakeIPs(config.Upstreams)

	return config, nil
}

// Start HTTP server.
func (c *Config) Start() {
	if c.log == nil {
		c.SetupLogs()
	}

	c.dispatch = server.NewServer(c.Config)
	smx := http.NewServeMux()
	apache, _ := apachelog.New(c.ApacheLogFormat())

	smx.Handle("/metrics", apache.Wrap(c.ValidateUpstream(promhttp.Handler()), c.httpLog.Writer()))
	smx.Handle("/register", c.dispatch.HandleRegister()) // apache log can't do websockets.
	smx.Handle("/stats", apache.Wrap(c.ValidateUpstream(http.HandlerFunc(c.dispatch.HandleStats)), c.httpLog.Writer()))
	smx.Handle("/request", apache.Wrap(http.HandlerFunc(c.HandleAll), c.httpLog.Writer())) // handleAll
	smx.Handle("/request/", apache.Wrap(http.StripPrefix("/request",
		c.ValidateUpstream(c.parsePath())), c.httpLog.Writer()))
	smx.Handle("/health", apache.Wrap(http.HandlerFunc(c.HandleOK), c.httpLog.Writer()))
	smx.Handle("/", apache.Wrap(http.HandlerFunc(c.HandleAll), c.httpLog.Writer()))

	var tlsConfig *tls.Config

	// Create TLS certificates if a Cache dir, CF Token and SSL Names are provided.
	if c.CacheDir != "" && len(c.SSLNames) > 0 && c.CFToken != "" {
		certmagic.DefaultACME.Email = c.Email
		certmagic.DefaultACME.Agreed = true
		certmagic.Default.Storage = &certmagic.FileStorage{Path: c.CacheDir}
		certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{APIToken: c.CFToken},
			},
		}

		var err error
		if tlsConfig, err = certmagic.TLS(c.SSLNames); err != nil {
			log.Fatalln("CertMagic TLS config failed:", err)
		}
	}

	c.server = &http.Server{
		ErrorLog:    c.log,
		Addr:        c.ListenAddr,
		Handler:     smx,
		ReadTimeout: c.Timeout,
		TLSConfig:   tlsConfig,
	}

	// Dispatch connection from available pools to client requests.
	go c.dispatch.StartDispatcher()
	// In a separate thread from the server thread.
	go c.runWebServer()
}

// parsePath is an assumption built for notifiarr.
// This uses a portion of the path as a label so
// we can see response times of our requests per api endpoint.
// We only use this tunnel to talk to 1 app, so the api paths we hit are bounded.
// This method chops the end off to avoid unbounded items in the URI.
func (c *Config) parsePath() http.HandlerFunc { //nolint:cyclop
	//nolint:mnd // Examples:
	//   /api/radarr/1/get/<random>
	//   /api/triggers/command/<random>
	return func(resp http.ResponseWriter, req *http.Request) {
		switch path := strings.SplitN(req.URL.Path, "/", 6); {
		case len(path) <= 1:
			c.dispatch.HandleRequest("/").ServeHTTP(resp, req)
		case path[1] != "api":
			c.dispatch.HandleRequest("/non/api").ServeHTTP(resp, req)
		case len(path) <= 3:
			c.dispatch.HandleRequest(req.URL.Path).ServeHTTP(resp, req)
		case path[2] == "trigger" || len(path) == 4:
			c.dispatch.HandleRequest(strings.Join(path[:4], "/")).ServeHTTP(resp, req)
		case path[3] == "2" || path[3] == "3" || path[3] == "4" || path[3] == "5":
			path[3] = "1"
			fallthrough
		default:
			c.dispatch.HandleRequest(strings.Join(path[:5], "/")).ServeHTTP(resp, req)
		}
	}
}

func (c *Config) runWebServer() {
	var err error

	if c.server.TLSConfig != nil {
		err = c.server.ListenAndServeTLS("", "")
	} else {
		err = c.server.ListenAndServe()
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalln("Web server failed, exiting:", err)
	}
}

func (c *Config) Shutdown() {
	c.dispatch.Shutdown()
}

// KeyValidator validates client secret keys against an nginx auth proxy.
// The actual auth proxy is: http://github.com/Notifiarr/mysql-auth-proxy
func (c *Config) KeyValidator(ctx context.Context, header http.Header) (string, error) {
	key := header.Get(mulch.SecretKeyHeader)
	if key == "" || len(key) != keyLen {
		return "", fmt.Errorf("%w: keyLen: %d!=%d", ErrInvalidKey, len(key), keyLen)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AuthURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating auth proxy request: %w", err)
	}

	req.Header.Add(c.AuthHeader, key)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("connecting to auth proxy: %w", err)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body) // avoid memory leak

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: status: %s", ErrInvalidKey, resp.Status)
	}

	return key, nil
}
