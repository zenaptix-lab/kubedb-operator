/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package healthz

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/cert"
)

// HealthzChecker is a named healthz checker.
type HealthzChecker interface {
	Name() string
	Check(req *http.Request) error
}

var defaultHealthz = sync.Once{}

// DefaultHealthz installs the default healthz check to the http.DefaultServeMux.
func DefaultHealthz(checks ...HealthzChecker) {
	defaultHealthz.Do(func() {
		InstallHandler(http.DefaultServeMux, checks...)
	})
}

// PingHealthz returns true automatically when checked
var PingHealthz HealthzChecker = ping{}

// ping implements the simplest possible healthz checker.
type ping struct{}

func (ping) Name() string {
	return "ping"
}

// PingHealthz is a health check that returns true.
func (ping) Check(_ *http.Request) error {
	return nil
}

// LogHealthz returns true if logging is not blocked
var LogHealthz HealthzChecker = &log{}

type log struct {
	startOnce    sync.Once
	lastVerified atomic.Value
}

func (l *log) Name() string {
	return "log"
}

func (l *log) Check(_ *http.Request) error {
	l.startOnce.Do(func() {
		l.lastVerified.Store(time.Now())
		go wait.Forever(func() {
			glog.Flush()
			l.lastVerified.Store(time.Now())
		}, time.Minute)
	})

	lastVerified := l.lastVerified.Load().(time.Time)
	if time.Since(lastVerified) < (2 * time.Minute) {
		return nil
	}
	return fmt.Errorf("logging blocked")
}

// NamedCheck returns a healthz checker for the given name and function.
func NamedCheck(name string, check func(r *http.Request) error) HealthzChecker {
	return &healthzCheck{name, check}
}

// InstallHandler registers handlers for health checking on the path
// "/healthz" to mux. *All handlers* for mux must be specified in
// exactly one call to InstallHandler. Calling InstallHandler more
// than once for the same mux will result in a panic.
func InstallHandler(mux mux, checks ...HealthzChecker) {
	InstallPathHandler(mux, "/healthz", checks...)
}

// InstallPathHandler registers handlers for health checking on
// a specific path to mux. *All handlers* for the path must be
// specified in exactly one call to InstallPathHandler. Calling
// InstallPathHandler more than once for the same path and mux will
// result in a panic.
func InstallPathHandler(mux mux, path string, checks ...HealthzChecker) {
	if len(checks) == 0 {
		glog.V(5).Info("No default health checks specified. Installing the ping handler.")
		checks = []HealthzChecker{PingHealthz}
	}

	glog.V(5).Info("Installing healthz checkers:", strings.Join(checkerNames(checks...), ", "))

	mux.Handle(path, handleRootHealthz(checks...))
	for _, check := range checks {
		mux.Handle(fmt.Sprintf("%s/%v", path, check.Name()), adaptCheckToHandler(check.Check))
	}
}

// mux is an interface describing the methods InstallHandler requires.
type mux interface {
	Handle(pattern string, handler http.Handler)
}

// healthzCheck implements HealthzChecker on an arbitrary name and check function.
type healthzCheck struct {
	name  string
	check func(r *http.Request) error
}

var _ HealthzChecker = &healthzCheck{}

func (c *healthzCheck) Name() string {
	return c.name
}

func (c *healthzCheck) Check(r *http.Request) error {
	return c.check(r)
}

// handleRootHealthz returns an http.HandlerFunc that serves the provided checks.
func handleRootHealthz(checks ...HealthzChecker) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failed := false
		var verboseOut bytes.Buffer
		for _, check := range checks {
			if err := check.Check(r); err != nil {
				// don't include the error since this endpoint is public.  If someone wants more detail
				// they should have explicit permission to the detailed checks.
				glog.V(6).Infof("healthz check %v failed: %v", check.Name(), err)
				fmt.Fprintf(&verboseOut, "[-]%v failed: reason withheld\n", check.Name())
				failed = true
			} else {
				fmt.Fprintf(&verboseOut, "[+]%v ok\n", check.Name())
			}
		}
		// always be verbose on failure
		if failed {
			http.Error(w, fmt.Sprintf("%vhealthz check failed", verboseOut.String()), http.StatusInternalServerError)
			return
		}

		if _, found := r.URL.Query()["verbose"]; !found {
			fmt.Fprint(w, "ok")
			return
		}

		verboseOut.WriteTo(w)
		fmt.Fprint(w, "healthz check passed\n")
	})
}

// adaptCheckToHandler returns an http.HandlerFunc that serves the provided checks.
func adaptCheckToHandler(c func(r *http.Request) error) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := c(r)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal server error: %v", err), http.StatusInternalServerError)
		} else {
			fmt.Fprint(w, "ok")
		}
	})
}

// checkerNames returns the names of the checks in the same order as passed in.
func checkerNames(checks ...HealthzChecker) []string {
	if len(checks) > 0 {
		// accumulate the names of checks for printing them out.
		checkerNames := make([]string, 0, len(checks))
		for _, check := range checks {
			// quote the Name so we can disambiguate
			checkerNames = append(checkerNames, fmt.Sprintf("%q", check.Name()))
		}
		return checkerNames
	}
	return nil
}

// CertHealthz returns true if tls.crt is unchanged when checked
func NewCertHealthz(certFile string) (HealthzChecker, error) {
	var hash string
	if certFile != "" {
		var err error
		hash, err = calculateHash(certFile)
		if err != nil {
			return nil, err
		}
	}
	return &certChecker{certFile: certFile, initialHash: hash}, nil
}

// certChecker fails health check if server certificate changes.
type certChecker struct {
	certFile    string
	initialHash string
}

func (certChecker) Name() string {
	return "cert-checker"
}

// CertHealthz is a health check that returns true.
func (c certChecker) Check(_ *http.Request) error {
	if c.certFile == "" {
		return nil
	}
	hash, err := calculateHash(c.certFile)
	if err != nil {
		return err
	}
	if c.initialHash != hash {
		return fmt.Errorf("certificate hash changed from %s to %s", c.initialHash, hash)
	}
	return nil
}

func calculateHash(certFile string) (string, error) {
	crtBytes, err := ioutil.ReadFile(certFile)
	if err != nil {
		return "", fmt.Errorf("failed to read certificate `%s`. Reason: %v", certFile, err)
	}
	crt, err := cert.ParseCertsPEM(crtBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse certificate `%s`. Reason: %v", certFile, err)
	}
	return Hash(crt[0]), nil
}

// ref: https://github.com/kubernetes/kubernetes/blob/197fc67693c2391dcbc652fc185ba85b5ef82a8e/cmd/kubeadm/app/util/pubkeypin/pubkeypin.go#L77

const (
	// formatSHA256 is the prefix for pins that are full-length SHA-256 hashes encoded in base 16 (hex)
	formatSHA256 = "sha256"
)

// Hash calculates the SHA-256 hash of the Subject Public Key Information (SPKI)
// object in an x509 certificate (in DER encoding). It returns the full hash as a
// hex encoded string (suitable for passing to Set.Allow).
func Hash(certificate *x509.Certificate) string {
	// ref: https://tools.ietf.org/html/rfc5280#section-4.1.2.7
	spkiHash := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return formatSHA256 + ":" + strings.ToLower(hex.EncodeToString(spkiHash[:]))
}
