package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// tlsCertValidity is long because nothing verifies the certificate:
// workers connect with sslmode=require, which encrypts without checking
// it, as jobtracker's do. The tailnet authenticates the peer.
const tlsCertValidity = 10 * 365 * 24 * time.Hour

// tlsCertCmd is `orbit tls-cert --dir <d> [--owner uid:gid] [--name cn]`:
// a self-signed server certificate for orbit-db's TLS, written once
// (server.crt, server.key) and kept on later runs. It exists because the
// postgres alpine image has no openssl; a one-shot service runs it before
// the database starts (docs/agents/deployment.md, Worker fleet).
func tlsCertCmd(args []string) error {
	fset := flag.NewFlagSet("tls-cert", flag.ContinueOnError)
	dir := fset.String("dir", "", "where to write server.crt and server.key")
	owner := fset.String("owner", "", "uid:gid to own both files (postgres refuses a key it does not own)")
	name := fset.String("name", "orbit-db", "the certificate's common name")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("tls-cert: --dir is required")
	}
	crt, key := filepath.Join(*dir, "server.crt"), filepath.Join(*dir, "server.key")
	if _, err := os.Stat(key); err == nil {
		fmt.Println("tls-cert: kept the existing certificate")
		return chownBoth(*owner, crt, key)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: *name}, DNSNames: []string{*name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(tlsCertValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &pk.PublicKey, pk)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(pk)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // a public certificate
		return err
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	fmt.Println("tls-cert: wrote", crt, "and", key)
	return chownBoth(*owner, crt, key)
}

func chownBoth(owner string, paths ...string) error {
	if owner == "" {
		return nil
	}
	u, g, ok := strings.Cut(owner, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil {
		return fmt.Errorf("tls-cert: --owner %q: want uid:gid", owner)
	}
	for _, p := range paths {
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}
