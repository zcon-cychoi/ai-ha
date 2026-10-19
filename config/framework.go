package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-barista/ai-ha/pkg/agent/transport"
)

// FrameworkConfig holds runtime configuration for the AI-HA framework server,
// loaded from AIHA_FRAMEWORK_* environment variables.
type FrameworkConfig struct {
	// Port is the TCP port the HTTPS server listens on.
	Port int
	// TLSCertFile and TLSKeyFile are the server certificate and key (PEM).
	TLSCertFile string
	TLSKeyFile  string
	// CertPin says how agents are told to verify this framework. True: they pin
	// the SHA-256 of the certificate's DER bytes, for a self-signed
	// certificate. False: they verify it against their CA bundle and the host
	// name in PublicURL, for a publicly trusted certificate. It decides what
	// "agent token issue" and "restore create" print; the server itself serves
	// the same certificate either way.
	CertPin bool
	// DBDSN is the PostgreSQL connection string. It holds a password: never log it.
	DBDSN string
	// SecretKeyFile holds the key, as 64 hexadecimal digits, that encrypts the
	// secrets kept in the database: backup repository credentials. Only the
	// commands that read or write those secrets require it.
	SecretKeyFile string
	// PublicURL is the address agents reach the framework at (e.g. a
	// port-forwarded https://host:port), printed for the instance init script.
	PublicURL string
	// ClaimTTL is how long a new restore job accepts its first claim.
	ClaimTTL time.Duration
	// ClaimRedelivery is how long after a claim the same request key may fetch
	// the response again.
	ClaimRedelivery time.Duration
	// LogLevel is the zerolog level ("debug", "info", "warn", "error").
	LogLevel string
	// CORSOrigins are the browser origins allowed to call the API. Empty
	// leaves CORS off. Rescue agents are not browsers and do not need it.
	CORSOrigins []string
	// EnableSwagger serves the OpenAPI document at /aiha/swagger.json. Off by
	// default: it describes the API to anyone who can reach the port.
	EnableSwagger bool

	// JobTTL is how long a job waits for its agent before it is failed.
	JobTTL time.Duration
	// JobClaimedTimeout is how long a claimed job may sit without reporting
	// that it started before it is returned to the queue.
	JobClaimedTimeout time.Duration
	// JobLease is how long one progress report keeps an attempt's lease. It
	// has to comfortably exceed JobProgressInterval, or an agent doing nothing
	// wrong loses its lease to one slow round trip.
	JobLease time.Duration
	// JobProgressInterval is how often agents are told to report.
	JobProgressInterval time.Duration
	// JobSweepInterval is how often timed-out jobs are moved on.
	JobSweepInterval time.Duration
}

// LoadFramework reads the framework configuration from environment variables
// (AIHA_FRAMEWORK_*), applying defaults for any value not set.
func LoadFramework() (*FrameworkConfig, error) {
	v := viper.New()
	v.SetEnvPrefix("AIHA_FRAMEWORK")
	v.AutomaticEnv()

	v.SetDefault("port", 8443)
	v.SetDefault("claim_ttl", 30*time.Minute)
	v.SetDefault("claim_redelivery", 10*time.Minute)
	v.SetDefault("log_level", "info")
	v.SetDefault("swagger", false)
	v.SetDefault("job_ttl", 24*time.Hour)
	v.SetDefault("job_claimed_timeout", 5*time.Minute)
	v.SetDefault("job_lease", 90*time.Second)
	v.SetDefault("job_progress_interval", 10*time.Second)
	v.SetDefault("job_sweep_interval", 30*time.Second)

	certPin, err := transport.ParseCertPin(v.GetString("cert_pin"), "AIHA_FRAMEWORK_CERT_PIN")
	if err != nil {
		return nil, err
	}

	return &FrameworkConfig{
		Port:            v.GetInt("port"),
		TLSCertFile:     v.GetString("tls_cert"),
		TLSKeyFile:      v.GetString("tls_key"),
		CertPin:         certPin,
		DBDSN:           v.GetString("db_dsn"),
		SecretKeyFile:   v.GetString("secret_key_file"),
		PublicURL:       v.GetString("public_url"),
		ClaimTTL:        v.GetDuration("claim_ttl"),
		ClaimRedelivery: v.GetDuration("claim_redelivery"),
		LogLevel:        v.GetString("log_level"),
		CORSOrigins:     splitList(v.GetString("cors_origins")),
		EnableSwagger:   v.GetBool("swagger"),

		JobTTL:              v.GetDuration("job_ttl"),
		JobClaimedTimeout:   v.GetDuration("job_claimed_timeout"),
		JobLease:            v.GetDuration("job_lease"),
		JobProgressInterval: v.GetDuration("job_progress_interval"),
		JobSweepInterval:    v.GetDuration("job_sweep_interval"),
	}, nil
}

// splitList parses a comma-separated setting into its entries, dropping empty
// ones. viper's GetStringSlice splits an environment value on whitespace
// rather than commas, so it cannot read a list of origins.
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
