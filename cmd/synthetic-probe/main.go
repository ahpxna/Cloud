// Command synthetic-probe exercises the public product path end to end with a
// dedicated probe account: login, resumable tus upload, verifier polling,
// library lookup, authenticated download, and SHA-256 verification.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

type sessionResponse struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	ExpectedSize   int64  `json:"expected_size"`
	ReceivedSize   int64  `json:"received_size"`
	UploadEndpoint string `json:"upload_endpoint"`
	SessionID      string `json:"session_id_metadata"`
	ChunkBytes     int64  `json:"recommended_chunk_bytes"`
	UploadToken    string `json:"upload_token"`
}

type asset struct {
	ID            string `json:"id"`
	ContentSHA256 string `json:"content_sha256"`
	OriginalURL   string `json:"original_url"`
}

type listResponse struct {
	Assets []asset `json:"assets"`
}

type probe struct {
	base       *url.URL
	client     *http.Client
	access     string
	chunkBytes int64
	chunkDelay time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "synthetic-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("synthetic-probe", flag.ContinueOnError)
	baseRaw := flags.String("base-url", os.Getenv("PROBE_BASE_URL"), "gateway base URL")
	email := flags.String("email", os.Getenv("PROBE_EMAIL"), "dedicated probe account email")
	passwordFile := flags.String("password-file", os.Getenv("PROBE_PASSWORD_FILE"), "file containing probe account password")
	fixtureBytes := flags.Int64("bytes", 1<<20, "synthetic fixture size")
	chunkBytes := flags.Int64("chunk-bytes", 1<<20, "PATCH size used by the probe")
	timeout := flags.Duration("timeout", 2*time.Minute, "overall probe timeout")
	chunkDelay := flags.Duration("chunk-delay", 0, "optional pause after every PATCH for chaos testing")
	allowHTTP := flags.Bool("allow-http", false, "permit plain HTTP for loopback-only development")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *baseRaw == "" || *email == "" || *passwordFile == "" {
		return errors.New("base URL, email, and password file are required")
	}
	if *fixtureBytes < 128 || *fixtureBytes > 256<<20 || *chunkBytes <= 0 || *chunkBytes > 32<<20 {
		return errors.New("fixture bytes must be 128 bytes..256 MiB and chunk bytes must be 1 byte..32 MiB")
	}
	base, err := url.Parse(*baseRaw)
	if err != nil || base.Host == "" || (base.Scheme != "https" && !(*allowHTTP && base.Scheme == "http")) {
		return errors.New("base URL must be HTTPS, or explicit loopback HTTP with -allow-http")
	}
	if base.Scheme == "http" && !isLoopbackHost(base.Hostname()) {
		return errors.New("plain HTTP probe is allowed only for loopback hosts")
	}
	passwordBytes, err := os.ReadFile(*passwordFile)
	if err != nil {
		return fmt.Errorf("read probe password: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))
	if password == "" {
		return errors.New("probe password file is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	p := &probe{
		base: base,
		client: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				if req.URL.Host != base.Host {
					return errors.New("refusing cross-host redirect")
				}
				return nil
			},
		},
		chunkBytes: *chunkBytes,
		chunkDelay: *chunkDelay,
	}
	fixture := makeFixture(*fixtureBytes)
	digest := sha256.Sum256(fixture)
	digestHex := hex.EncodeToString(digest[:])
	if err := p.login(ctx, *email, password); err != nil {
		return err
	}
	session, err := p.createSession(ctx, fixture, digestHex)
	if err != nil {
		return err
	}
	if session.State != "available" {
		if err := p.resumeUpload(ctx, session, fixture); err != nil {
			return err
		}
		if err := p.waitAvailable(ctx, session.ID); err != nil {
			return err
		}
	}
	originalURL, err := p.findAsset(ctx, digestHex)
	if err != nil {
		return err
	}
	if err := p.verifyDownload(ctx, originalURL, digest); err != nil {
		return err
	}
	fmt.Printf("PASS synthetic upload/download integrity probe (%d bytes, sha256=%s)\n", len(fixture), digestHex)
	return nil
}

func (p *probe) login(ctx context.Context, email, password string) error {
	payload := map[string]string{"email": email, "password": password, "device_name": "synthetic-integrity-probe"}
	var response tokenResponse
	if err := p.jsonRequest(ctx, http.MethodPost, "/v1/auth/login", "", payload, &response); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if response.AccessToken == "" {
		return errors.New("login returned no access token")
	}
	p.access = response.AccessToken
	return nil
}

func (p *probe) createSession(ctx context.Context, fixture []byte, digest string) (sessionResponse, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return sessionResponse{}, fmt.Errorf("generate probe session nonce: %w", err)
	}
	clientID := "synthetic-probe-v1-" + strconv.Itoa(len(fixture)) + "-" + hex.EncodeToString(nonce)
	payload := map[string]any{
		"client_asset_id":   clientID,
		"original_filename": "synthetic-probe.png",
		"media_type":        "image/png",
		"expected_size":     len(fixture),
		"client_sha256":     digest,
	}
	var response sessionResponse
	err := p.jsonRequest(ctx, http.MethodPost, "/v1/upload-sessions", p.access, payload, &response)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("create upload session: %w", err)
	}
	return response, nil
}

func (p *probe) resumeUpload(ctx context.Context, session sessionResponse, fixture []byte) error {
	fresh, err := p.getSession(ctx, session.ID)
	if err != nil {
		return err
	}
	session = fresh
	switch session.State {
	case "created":
		if err := p.createTusResource(ctx, session); err != nil {
			return err
		}
	case "uploading":
	case "received", "verifying", "verified", "committing", "available":
		return nil
	default:
		return fmt.Errorf("upload session is in terminal state %q", session.State)
	}
	offset, err := p.headOffset(ctx, session)
	if err != nil {
		return err
	}
	for offset < int64(len(fixture)) {
		fresh, err := p.getSession(ctx, session.ID)
		if err != nil {
			return err
		}
		session.UploadToken = fresh.UploadToken
		end := offset + p.chunkBytes
		if end > int64(len(fixture)) {
			end = int64(len(fixture))
		}
		if err := p.patch(ctx, session, offset, fixture[offset:end]); err != nil {
			// One retry path proves resume semantics instead of assuming a
			// failed PATCH wrote no bytes.
			fresh, getErr := p.getSession(ctx, session.ID)
			if getErr != nil {
				return fmt.Errorf("PATCH failed (%v) and session refresh failed: %w", err, getErr)
			}
			session = fresh
			head, headErr := p.headOffset(ctx, session)
			if headErr != nil {
				return fmt.Errorf("PATCH failed (%v) and HEAD failed: %w", err, headErr)
			}
			offset = head
			continue
		}
		offset = end
		if p.chunkDelay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.chunkDelay):
			}
		}
	}
	return nil
}

func (p *probe) createTusResource(ctx context.Context, session sessionResponse) error {
	endpoint := session.UploadEndpoint
	if endpoint == "" {
		endpoint = "/v1/uploads/"
	}
	request, err := p.request(ctx, http.MethodPost, endpoint, session.UploadToken, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Length", strconv.FormatInt(session.ExpectedSize, 10))
	request.Header.Set("Upload-Metadata", "session_id "+base64.StdEncoding.EncodeToString([]byte(session.ID)))
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("create tus resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return statusError("create tus resource", response)
	}
	return nil
}

func (p *probe) headOffset(ctx context.Context, session sessionResponse) (int64, error) {
	request, err := p.request(ctx, http.MethodHead, "/v1/uploads/"+session.ID, session.UploadToken, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Tus-Resumable", "1.0.0")
	response, err := p.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("HEAD tus resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return 0, statusError("HEAD tus resource", response)
	}
	offset, err := strconv.ParseInt(response.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 || offset > session.ExpectedSize {
		return 0, errors.New("invalid Upload-Offset from server")
	}
	return offset, nil
}

func (p *probe) patch(ctx context.Context, session sessionResponse, offset int64, content []byte) error {
	request, err := p.request(ctx, http.MethodPatch, "/v1/uploads/"+session.ID, session.UploadToken, bytes.NewReader(content))
	if err != nil {
		return err
	}
	request.ContentLength = int64(len(content))
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("PATCH tus resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return statusError("PATCH tus resource", response)
	}
	return nil
}

func (p *probe) getSession(ctx context.Context, id string) (sessionResponse, error) {
	var response sessionResponse
	if err := p.jsonRequest(ctx, http.MethodGet, "/v1/upload-sessions/"+id, p.access, nil, &response); err != nil {
		return sessionResponse{}, fmt.Errorf("get upload session: %w", err)
	}
	return response, nil
}

func (p *probe) waitAvailable(ctx context.Context, id string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		session, err := p.getSession(ctx, id)
		if err != nil {
			return err
		}
		switch session.State {
		case "available":
			return nil
		case "quarantined", "failed", "expired":
			return fmt.Errorf("upload reached terminal state %q", session.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *probe) findAsset(ctx context.Context, digest string) (string, error) {
	var response listResponse
	if err := p.jsonRequest(ctx, http.MethodGet, "/v1/assets?limit=100", p.access, nil, &response); err != nil {
		return "", err
	}
	for _, item := range response.Assets {
		if strings.EqualFold(item.ContentSHA256, digest) {
			if item.OriginalURL == "" {
				return "", errors.New("matching asset has no original URL")
			}
			return item.OriginalURL, nil
		}
	}
	return "", errors.New("verified probe asset not visible in library")
}

func (p *probe) verifyDownload(ctx context.Context, path string, expected [32]byte) error {
	request, err := p.request(ctx, http.MethodGet, path, p.access, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return statusError("download probe asset", response)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(response.Body, 300<<20)); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], hasher.Sum(nil))
	if got != expected {
		return fmt.Errorf("download SHA-256 mismatch: got %s want %s", hex.EncodeToString(got[:]), hex.EncodeToString(expected[:]))
	}
	return nil
}

func (p *probe) jsonRequest(ctx context.Context, method, path, token string, body any, destination any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := p.request(ctx, method, path, token, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError(method+" "+path, response)
	}
	if destination == nil {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	return decoder.Decode(destination)
}

func (p *probe) request(ctx context.Context, method, path, token string, body io.Reader) (*http.Request, error) {
	target, err := p.base.Parse(path)
	if err != nil {
		return nil, err
	}
	if target.Host != p.base.Host {
		return nil, errors.New("request path escaped probe host")
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("User-Agent", "family-photo-cloud-synthetic-probe/1")
	return request, nil
}

func statusError(operation string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	return fmt.Errorf("%s: HTTP %d: %s", operation, response.StatusCode, strings.TrimSpace(string(body)))
}

func makeFixture(size int64) []byte {
	// Valid PNG signature/IHDR prefix followed by deterministic probe bytes.
	// The backend's integrity contract is byte-exact; this fixture is never
	// interpreted as family media and should live in a dedicated probe account.
	prefix, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	fixture := make([]byte, size)
	copy(fixture, prefix)
	for index := len(prefix); index < len(fixture); index++ {
		fixture[index] = byte((index*131 + 17) % 251)
	}
	return fixture
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
