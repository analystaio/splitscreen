package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/harness"
	"github.com/avarant/splitscreen/protocol"
)

// ---------------------------------------------------------------------------
// enroll
// ---------------------------------------------------------------------------

func enrollCmd() *cobra.Command {
	var cfgPath string
	var write, force, tokenStdin, printToken bool

	cmd := &cobra.Command{
		Use:   "enroll <runner>",
		Short: "Generate an enrollment token for a runner",
		Long: `Generates a fresh enrollment token and the secret name the gateway looks it
up under.

Run this on the gateway host with --write and it stores the gateway's half
itself, leaving only the runner's half to carry across. That halves the number
of times the token is copied, which matters: a truncated paste fails later as an
opaque "enrollment token does not match" with nothing else to go on.

For automation:

  --token-stdin   use the token read from stdin instead of generating one, so a
                  caller that already holds it (and has put it wherever the
                  runner will fetch it) can store the gateway's half
  --print-token   print only the token, nothing else, for capture by a script

A caller that writes the token straight into the gateway's Parameter Store
prefix (<secrets_ssm.prefix>/runner-<name>) does not need this command at all:
the gateway reads it on the runner's next connection. Note that a file of the
same name in gateway.secrets_dir wins over Parameter Store.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runnerName := args[0]
			if !protocol.ValidSlug(runnerName) {
				return fmt.Errorf("splitscreen: %q is not a valid runner name (lowercase letters, digits, dashes)", runnerName)
			}
			token, err := enrollmentToken(tokenStdin)
			if err != nil {
				return err
			}
			secretName := "runner-" + runnerName

			var stored string
			if write {
				cfg, err := config.Load(cfgPath)
				if err != nil {
					return fmt.Errorf("--write needs a valid config to find the secret store:\n%w", err)
				}
				rc, ok := cfg.Runners[runnerName]
				if !ok {
					// The gateway refuses a hello from a runner it has no config
					// for, so enrolling one that does not exist yet only produces
					// a confusing failure later.
					return fmt.Errorf("no runner named %q is configured; add it to %s first", runnerName, cfgPath)
				}
				secretName = rc.EffectiveTokenSecret(runnerName)
				if cfg.Gateway.SecretsDir == "" {
					return fmt.Errorf("--write needs gateway.secrets_dir; for the ssm backend, put the value at %s/%s instead",
						cfg.Gateway.SecretsSSM.Prefix, secretName)
				}
				dest := filepath.Join(cfg.Gateway.SecretsDir, secretName)
				if _, err := os.Stat(dest); err == nil && !force {
					return fmt.Errorf("%s already exists; pass --force to replace it (every runner using the old token will be locked out)", dest)
				}
				if err := os.WriteFile(dest, []byte(token), 0o600); err != nil {
					return fmt.Errorf("splitscreen: write secret: %w", err)
				}
				stored = dest
			}

			if printToken {
				fmt.Println(token)
				return nil
			}

			fmt.Printf("runner:      %s\n", runnerName)
			fmt.Printf("secret name: %s\n", secretName)
			if stored != "" {
				fmt.Printf("stored at:   %s (mode 0600)\n", stored)
			}
			fmt.Println()

			if stored == "" {
				fmt.Printf("On the gateway:\n")
				fmt.Printf("  printf %%s '%s' > $SECRETS_DIR/%s && chmod 600 $SECRETS_DIR/%s\n\n",
					token, secretName, secretName)
			}
			if !tokenStdin {
				fmt.Printf("On the runner:\n")
				fmt.Printf("  printf %%s '%s' > ~/.config/splitscreen/%s.token\n", token, runnerName)
				fmt.Printf("  chmod 600 ~/.config/splitscreen/%s.token\n", runnerName)
				fmt.Printf("  systemctl --user enable --now splitscreen-runner@%s\n\n", runnerName)
			}
			if stored != "" {
				fmt.Printf("The gateway picks up the new secret on its next read; no reload is needed\n")
				fmt.Printf("unless you also changed the config.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().BoolVar(&write, "write", false, "store the gateway's half in the configured secrets directory")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing secret")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from stdin instead of generating one")
	cmd.Flags().BoolVar(&printToken, "print-token", false, "print only the token")
	return cmd
}

// minTokenLen rejects tokens too short to be worth the name. A generated one is
// 43 characters; a supplied one must at least not be guessable.
const minTokenLen = 32

// enrollmentToken generates a token, or reads one supplied on stdin. Stdin rather
// than a flag, because argv is visible to every user on the box and lands in
// shell history and remote-command logs.
func enrollmentToken(fromStdin bool) (string, error) {
	if !fromStdin {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw[:]), nil
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return "", fmt.Errorf("splitscreen: read token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < minTokenLen {
		return "", fmt.Errorf("splitscreen: the token on stdin is %d characters; at least %d are required", len(token), minTokenLen)
	}
	if strings.ContainsAny(token, " \t\r\n'\"") {
		return "", errors.New("splitscreen: the token on stdin contains whitespace or quotes")
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// config check
// ---------------------------------------------------------------------------

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}

	var cfgPath string
	check := &cobra.Command{
		Use:   "check",
		Short: "Validate a configuration file",
		Long: `Validates a configuration file and reports every problem at once.

This is the same code path the gateway uses on load and on SIGHUP, so a config
that passes here will not be rejected at reload.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			fmt.Printf("%s is valid.\n", cfgPath)
			for _, w := range cfg.Warnings {
				fmt.Printf("  warning: %s\n", w)
			}
			fmt.Println()

			names := make([]string, 0, len(cfg.Runners))
			for n := range cfg.Runners {
				names = append(names, n)
			}
			sort.Strings(names)

			fmt.Printf("Runners (%d):\n", len(names))
			for _, n := range names {
				r := cfg.Runners[n]
				fmt.Printf("  %-20s harness=%-12s cwd=%s idle=%s\n", n, r.Harness, r.Cwd, r.Idle)
			}

			fmt.Printf("\nRoutes (%d):\n", len(cfg.Routes))
			for _, r := range cfg.Routes {
				target := r.Channel
				if r.DM {
					target = "<direct messages>"
				}
				fmt.Printf("  %-20s -> %s\n", target, r.Runner)
			}

			if proxied := cfg.ProxiedServers(); len(proxied) > 0 {
				sort.Strings(proxied)
				fmt.Printf("\nProxied MCP servers: %v\n", proxied)
			}

			refs := cfg.SecretRefs()
			sort.Strings(refs)
			fmt.Printf("\nSecrets this config expects (%d): %v\n", len(refs), refs)
			fmt.Printf("Harness adapters available: %v\n", harness.Names())
			return nil
		},
	}
	check.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")

	cmd.AddCommand(check)
	return cmd
}

// ---------------------------------------------------------------------------
// cert
// ---------------------------------------------------------------------------

func certCmd() *cobra.Command {
	var (
		certPath, keyPath string
		hosts             []string
		days              int
	)

	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Generate a self-signed certificate for the gateway",
		Long: `Generates a self-signed certificate and prints its fingerprint.

Public CAs will not issue for a private address, and a private CA is
disproportionate for a small fleet. Runners pin the fingerprint printed here,
which is a stronger check than chain validation for a two-node deployment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(hosts) == 0 {
				return fmt.Errorf("splitscreen: at least one --host is required")
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return err
			}
			serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
			if err != nil {
				return err
			}
			tmpl := x509.Certificate{
				SerialNumber:          serial,
				Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"Splitscreen"}},
				NotBefore:             time.Now().Add(-time.Hour),
				NotAfter:              time.Now().AddDate(0, 0, days),
				KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
				ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				BasicConstraintsValid: true,
				IsCA:                  true,
			}
			for _, h := range hosts {
				if ip := net.ParseIP(h); ip != nil {
					tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
				} else {
					tmpl.DNSNames = append(tmpl.DNSNames, h)
				}
			}

			der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
			if err != nil {
				return err
			}
			certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			keyDER, err := x509.MarshalECPrivateKey(key)
			if err != nil {
				return err
			}
			keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

			if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
				return err
			}

			sum := sha256.Sum256(der)
			fmt.Printf("certificate: %s\n", certPath)
			fmt.Printf("private key: %s (mode 0600)\n", keyPath)
			fmt.Printf("hosts:       %v\n", hosts)
			fmt.Printf("expires:     %s\n\n", tmpl.NotAfter.Format(time.RFC3339))
			fmt.Printf("Pin this on every runner:\n")
			fmt.Printf("  --fingerprint sha256:%s\n", hex.EncodeToString(sum[:]))
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&certPath, "cert", "gateway.crt", "certificate output path")
	f.StringVar(&keyPath, "key", "gateway.key", "private key output path")
	f.StringSliceVar(&hosts, "host", nil, "DNS name or IP the gateway is reached at (repeatable)")
	f.IntVar(&days, "days", 825, "validity in days")
	return cmd
}
