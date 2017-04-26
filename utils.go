/*
Copyright 2015 All rights reserved.
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

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc/jose"
	"github.com/coreos/go-oidc/oidc"
	"github.com/labstack/echo"
	"github.com/urfave/cli"
	"gopkg.in/yaml.v2"
)

var (
	allHTTPMethods = []string{
		echo.DELETE,
		echo.GET,
		echo.HEAD,
		echo.OPTIONS,
		echo.PATCH,
		echo.POST,
		echo.PUT,
		echo.TRACE,
	}
)

var (
	symbolsFilter = regexp.MustCompilePOSIX("[_$><\\[\\].,\\+-/'%^&*()!\\\\]+")
)

// readConfigFile reads and parses the configuration file
func readConfigFile(filename string, config *Config) error {
	// step: read in the contents of the file
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	// step: attempt to un-marshal the data
	switch ext := filepath.Ext(filename); ext {
	case "json":
		err = json.Unmarshal(content, config)
	default:
		err = yaml.Unmarshal(content, config)
	}

	return err
}

// encryptDataBlock encrypts the plaintext string with the key
func encryptDataBlock(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return []byte{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return []byte{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decryptDataBlock decrypts some cipher text
func decryptDataBlock(cipherText, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return []byte{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return []byte{}, err
	}
	nonceSize := gcm.NonceSize()
	if len(cipherText) < nonceSize {
		return nil, errors.New("failed to decrypt the ciphertext, the text is too short")
	}
	nonce, input := cipherText[:nonceSize], cipherText[nonceSize:]

	return gcm.Open(nil, nonce, input, nil)
}

// encodeText encodes the session state information into a value for a cookie to consume
func encodeText(plaintext string, key string) (string, error) {
	cipherText, err := encryptDataBlock([]byte(plaintext), []byte(key))
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(cipherText), nil
}

// decodeText decodes the session state cookie value
func decodeText(state, key string) (string, error) {
	cipherText, err := hex.DecodeString(state)
	if err != nil {
		return "", err
	}
	// step: decrypt the cookie back in the expiration|token
	encoded, err := decryptDataBlock(cipherText, []byte(key))
	if err != nil {
		return "", ErrInvalidSession
	}

	return string(encoded), nil
}

// newOpenIDClient initializes the openID configuration, note: the redirection url is deliberately left blank
// in order to retrieve it from the host header on request
func newOpenIDClient(cfg *Config) (*oidc.Client, oidc.ProviderConfig, *http.Client, error) {
	var err error
	var config oidc.ProviderConfig

	// step: fix up the url if required, the underlining lib will add the .well-known/openid-configuration to the discovery url for us.
	if strings.HasSuffix(cfg.DiscoveryURL, "/.well-known/openid-configuration") {
		cfg.DiscoveryURL = strings.TrimSuffix(cfg.DiscoveryURL, "/.well-known/openid-configuration")
	}

	// step: create a idp http client
	hc := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: cfg.SkipOpenIDProviderTLSVerify,
			},
		},
		Timeout: time.Second * 10,
	}

	// step: attempt to retrieve the provider configuration
	completeCh := make(chan bool)
	go func() {
		for {
			log.Infof("attempting to retrieve openid configuration from discovery url: %s", cfg.DiscoveryURL)
			if config, err = oidc.FetchProviderConfig(hc, cfg.DiscoveryURL); err == nil {
				break // break and complete
			}
			log.Warnf("failed to get provider configuration from discovery url: %s, %s", cfg.DiscoveryURL, err)
			time.Sleep(time.Second * 3)
		}
		completeCh <- true
	}()
	// step: wait for timeout or successful retrieval
	select {
	case <-time.After(30 * time.Second):
		return nil, config, nil, errors.New("failed to retrieve the provider configuration from discovery url")
	case <-completeCh:
		log.Infof("successfully retrieved the openid configuration from the discovery url: %s", cfg.DiscoveryURL)
	}

	client, err := oidc.NewClient(oidc.ClientConfig{
		ProviderConfig: config,
		Credentials: oidc.ClientCredentials{
			ID:     cfg.ClientID,
			Secret: cfg.ClientSecret,
		},
		RedirectURL: fmt.Sprintf("%s/oauth/callback", cfg.RedirectionURL),
		Scope:       append(cfg.Scopes, oidc.DefaultScope...),
		HTTPClient:  hc,
	})
	if err != nil {
		return nil, config, hc, err
	}

	// step: start the provider sync for key rotation
	client.SyncProviderConfig(cfg.DiscoveryURL)

	return client, config, hc, nil
}

// decodeKeyPairs converts a list of strings (key=pair) to a map
func decodeKeyPairs(list []string) (map[string]string, error) {
	kp := make(map[string]string)

	for _, x := range list {
		items := strings.Split(x, "=")
		if len(items) != 2 {
			return kp, fmt.Errorf("invalid tag '%s' should be key=pair", x)
		}
		kp[items[0]] = items[1]
	}

	return kp, nil
}

// isValidHTTPMethod ensure this is a valid http method type
func isValidHTTPMethod(method string) bool {
	for _, x := range allHTTPMethods {
		if method == x {
			return true
		}
	}

	return false
}

// defaultTo returns the value of the default
func defaultTo(v, d string) string {
	if v != "" {
		return v
	}

	return d
}

// fileExists check if a file exists
func fileExists(filename string) bool {
	if _, err := os.Stat(filename); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// hasRoles checks the scopes are the same
func hasRoles(required, issued []string) bool {
	for _, role := range required {
		if !containedIn(role, issued) {
			return false
		}
	}

	return true
}

// containedIn checks if a value in a list of a strings
func containedIn(value string, list []string) bool {
	for _, x := range list {
		if x == value {
			return true
		}
	}

	return false
}

// containsSubString checks if substring exists
func containsSubString(value string, list []string) bool {
	for _, x := range list {
		if strings.Contains(value, x) {
			return true
		}
	}

	return false
}

// tryDialEndpoint dials the upstream endpoint via plain
func tryDialEndpoint(location *url.URL) (net.Conn, error) {
	switch dialAddress := dialAddress(location); location.Scheme {
	case httpSchema:
		return net.Dial("tcp", dialAddress)
	default:
		return tls.Dial("tcp", dialAddress, &tls.Config{
			Rand:               rand.Reader,
			InsecureSkipVerify: true,
		})
	}
}

// isUpgradedConnection checks to see if the request is requesting
func isUpgradedConnection(req *http.Request) bool {
	return req.Header.Get(headerUpgrade) != ""
}

// transferBytes transfers bytes between the sink and source
func transferBytes(src io.Reader, dest io.Writer, wg *sync.WaitGroup) (int64, error) {
	defer wg.Done()
	return io.Copy(dest, src)
}

// tryUpdateConnection attempt to upgrade the connection to a http pdy stream
func tryUpdateConnection(req *http.Request, writer http.ResponseWriter, endpoint *url.URL) error {
	// step: dial the endpoint
	tlsConn, err := tryDialEndpoint(endpoint)
	if err != nil {
		return err
	}
	defer tlsConn.Close()

	// step: we need to hijack the underlining client connection
	clientConn, _, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		return err
	}
	defer clientConn.Close()

	// step: write the request to upstream
	if err = req.Write(tlsConn); err != nil {
		return err
	}

	// step: copy the date between client and upstream endpoint
	var wg sync.WaitGroup
	wg.Add(2)
	go transferBytes(tlsConn, clientConn, &wg)
	go transferBytes(clientConn, tlsConn, &wg)
	wg.Wait()

	return nil
}

// dialAddress extracts the dial address from the url
func dialAddress(location *url.URL) string {
	items := strings.Split(location.Host, ":")
	if len(items) != 2 {
		switch location.Scheme {
		case httpSchema:
			return location.Host + ":80"
		default:
			return location.Host + ":443"
		}
	}

	return location.Host
}

// findCookie looks for a cookie in a list of cookies
func findCookie(name string, cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}

	return nil
}

// toHeader is a helper method to play nice in the headers
func toHeader(v string) string {
	var list []string

	// step: filter out any symbols and convert to dashes
	for _, x := range symbolsFilter.Split(v, -1) {
		list = append(list, capitalize(x))
	}

	return strings.Join(list, "-")
}

// capitalize capitalizes the first letter of a word
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)

	return string(unicode.ToUpper(r)) + s[n:]
}

// mergeMaps simples copies the keys from source to destination
func mergeMaps(dest, source map[string]string) map[string]string {
	for k, v := range source {
		dest[k] = v
	}

	return dest
}

// loadCA loads the certificate authority
func loadCA(cert, key string) (*tls.Certificate, error) {
	caCert, err := ioutil.ReadFile(cert)
	if err != nil {
		return nil, err
	}

	caKey, err := ioutil.ReadFile(key)
	if err != nil {
		return nil, err
	}

	ca, err := tls.X509KeyPair(caCert, caKey)
	if err != nil {
		return nil, err
	}

	ca.Leaf, err = x509.ParseCertificate(ca.Certificate[0])

	return &ca, err
}

// getWithin calculates a duration of x percent of the time period, i.e. something
// expires in 1 hours, get me a duration within 80%
func getWithin(expires time.Time, within float64) time.Duration {
	left := expires.UTC().Sub(time.Now().UTC()).Seconds()
	if left <= 0 {
		return time.Duration(0)
	}
	seconds := int(left * within)

	return time.Duration(seconds) * time.Second
}

// getHashKey returns a hash of the encodes jwt token
func getHashKey(token *jose.JWT) string {
	hash := md5.Sum([]byte(token.Encode()))
	return hex.EncodeToString(hash[:])
}

// printError display the command line usage and error
func printError(message string, args ...interface{}) *cli.ExitError {
	return cli.NewExitError(fmt.Sprintf("[error] "+message, args...), 1)
}
