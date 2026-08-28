package main

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"
)

type Config struct {
	Ergo      ErgoConfig      `json:"ergo"`
	RDirCD    RDirCDConfig    `json:"rdircd"`
	Channels  ChannelConfig   `json:"channels"`
	Reconnect ReconnectConfig `json:"reconnect"`
}

type ErgoConfig struct {
	Address          string   `json:"address"`
	TLSServerName    string   `json:"tls_server_name"`
	Nick             string   `json:"nick"`
	Account          string   `json:"account"`
	PasswordFile     string   `json:"password_file"`
	OperName         string   `json:"oper_name"`
	OperPasswordFile string   `json:"oper_password_file"`
	CAFile           string   `json:"ca_file"`
	OwnerAccounts    []string `json:"owner_accounts"`
}

type RDirCDConfig struct {
	Address string `json:"address"`
	Nick    string `json:"nick"`
}

type ChannelConfig struct {
	Prefix           string            `json:"prefix"`
	DMIncludePattern string            `json:"dm_include_pattern"`
	GuildInclude     []string          `json:"guild_include_globs"`
	Aliases          map[string]string `json:"channel_aliases"`
	CatchUp          bool              `json:"catch_up"`
	CatchUpLimit     int               `json:"catch_up_limit"`
	MaxNameLength    int               `json:"max_name_length"`
}

type ReconnectConfig struct {
	Minimum time.Duration `json:"-"`
	Maximum time.Duration `json:"-"`
	MinText string        `json:"minimum"`
	MaxText string        `json:"maximum"`
}

type RuntimeConfig struct {
	Config
	ErgoPassword     string
	OperPassword     string
	ErgoRootCAs      *x509.CertPool
	OwnerAccountsSet map[string]struct{}
}

func loadConfig(filename string) (RuntimeConfig, error) {
	f, err := os.Open(filename)
	if err != nil {
		return RuntimeConfig{}, err
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return RuntimeConfig{}, err
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return RuntimeConfig{}, err
	}

	ergoPassword, err := readSecret(cfg.Ergo.PasswordFile)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read Ergo SASL password: %w", err)
	}
	operPassword, err := readSecret(cfg.Ergo.OperPasswordFile)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read Ergo oper password: %w", err)
	}

	var roots *x509.CertPool
	if cfg.Ergo.CAFile != "" {
		ca, err := os.ReadFile(cfg.Ergo.CAFile)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("read Ergo CA file: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return RuntimeConfig{}, errors.New("Ergo CA file contains no certificates")
		}
	}

	owners := make(map[string]struct{}, len(cfg.Ergo.OwnerAccounts))
	for _, owner := range cfg.Ergo.OwnerAccounts {
		owners[ircCasefold(owner)] = struct{}{}
	}
	return RuntimeConfig{
		Config:           cfg,
		ErgoPassword:     ergoPassword,
		OperPassword:     operPassword,
		ErgoRootCAs:      roots,
		OwnerAccountsSet: owners,
	}, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing config data: %w", err)
	}
	return errors.New("config contains multiple JSON values")
}

func (cfg *Config) defaults() {
	if cfg.Ergo.Nick == "" {
		cfg.Ergo.Nick = "Dickord"
	}
	if cfg.Ergo.Account == "" {
		cfg.Ergo.Account = cfg.Ergo.Nick
	}
	if cfg.Ergo.OperName == "" {
		cfg.Ergo.OperName = cfg.Ergo.Nick
	}
	if cfg.RDirCD.Nick == "" {
		cfg.RDirCD.Nick = "dickord"
	}
	if cfg.Channels.Prefix == "" {
		cfg.Channels.Prefix = "discord."
	}
	cfg.Channels.Prefix = strings.TrimPrefix(cfg.Channels.Prefix, "#")
	if cfg.Channels.DMIncludePattern == "" {
		cfg.Channels.DMIncludePattern = "me.*"
	}
	if cfg.Channels.CatchUpLimit == 0 {
		cfg.Channels.CatchUpLimit = 300
	}
	if cfg.Channels.MaxNameLength == 0 {
		cfg.Channels.MaxNameLength = 64
	}
	if cfg.Reconnect.MinText == "" {
		cfg.Reconnect.MinText = "2s"
	}
	if cfg.Reconnect.MaxText == "" {
		cfg.Reconnect.MaxText = "1m"
	}
	cfg.Reconnect.Minimum, _ = time.ParseDuration(cfg.Reconnect.MinText)
	cfg.Reconnect.Maximum, _ = time.ParseDuration(cfg.Reconnect.MaxText)
}

func (cfg Config) validate() error {
	for name, address := range map[string]string{
		"ergo.address":   cfg.Ergo.Address,
		"rdircd.address": cfg.RDirCD.Address,
	} {
		if address == "" {
			return fmt.Errorf("%s is required", name)
		}
		if _, _, err := net.SplitHostPort(address); err != nil {
			return fmt.Errorf("%s must be host:port: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"ergo.tls_server_name":    cfg.Ergo.TLSServerName,
		"ergo.nick":               cfg.Ergo.Nick,
		"ergo.account":            cfg.Ergo.Account,
		"ergo.password_file":      cfg.Ergo.PasswordFile,
		"ergo.oper_name":          cfg.Ergo.OperName,
		"ergo.oper_password_file": cfg.Ergo.OperPasswordFile,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(cfg.Ergo.OwnerAccounts) == 0 {
		return errors.New("ergo.owner_accounts must contain at least one account")
	}
	for _, owner := range cfg.Ergo.OwnerAccounts {
		if strings.TrimSpace(owner) == "" {
			return errors.New("ergo.owner_accounts cannot contain an empty account")
		}
	}
	if strings.ContainsAny(cfg.Channels.Prefix, " ,:\r\n") {
		return errors.New("channels.prefix contains invalid IRC channel characters")
	}
	if cfg.Channels.MaxNameLength < 24 || cfg.Channels.MaxNameLength > 512 {
		return errors.New("channels.max_name_length must be between 24 and 512")
	}
	if cfg.Channels.CatchUpLimit < 1 || cfg.Channels.CatchUpLimit > 10000 {
		return errors.New("channels.catch_up_limit must be between 1 and 10000")
	}
	for source, alias := range cfg.Channels.Aliases {
		if strings.TrimSpace(source) == "" || strings.TrimSpace(alias) == "" {
			return errors.New("channels.channel_aliases cannot contain empty names")
		}
		if strings.ContainsAny(alias, " ,:\r\n") {
			return fmt.Errorf("channel alias %q contains invalid IRC channel characters", alias)
		}
	}
	for _, pattern := range append([]string{cfg.Channels.DMIncludePattern}, cfg.Channels.GuildInclude...) {
		if _, err := path.Match(pattern, "test"); err != nil {
			return fmt.Errorf("invalid channel glob %q: %w", pattern, err)
		}
	}
	if cfg.Reconnect.Minimum <= 0 || cfg.Reconnect.Maximum < cfg.Reconnect.Minimum {
		return errors.New("reconnect durations must be positive and maximum >= minimum")
	}
	return nil
}

func readSecret(filename string) (string, error) {
	value, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", errors.New("secret file is empty")
	}
	return secret, nil
}
