package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxAttachmentSize      = 100 << 20
	maxAttachmentURLLength = 400
)

var (
	discordAttachmentPath = regexp.MustCompile(`^/(attachments|ephemeral-attachments)/[0-9]+/[0-9]+/[^/]+$`)
	motdVoiceFallback     = regexp.MustCompile(`^\[voice ([0-9]+(?::[0-9]{2}){1,2}) (audio/[A-Za-z0-9!#$&^_.+-]+)(?: expires=([^\]\s]+))?\] (https://[^\s<>]+)$`)
)

type attachmentUploader struct {
	downloadClient *http.Client
	uploadClient   *http.Client
	account        string
	password       string
}

func newAttachmentUploader(cfg RuntimeConfig) *attachmentUploader {
	downloadTransport := http.DefaultTransport.(*http.Transport).Clone()
	downloadTransport.DisableCompression = true
	downloadTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	uploadTransport := downloadTransport.Clone()
	uploadTransport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    cfg.ErgoRootCAs,
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &attachmentUploader{
		downloadClient: &http.Client{Transport: downloadTransport, CheckRedirect: noRedirect},
		uploadClient:   &http.Client{Transport: uploadTransport, CheckRedirect: noRedirect},
		account:        cfg.Ergo.Account,
		password:       cfg.ErgoPassword,
	}
}

func (u *attachmentUploader) Upload(ctx context.Context, endpoint, source string) (string, error) {
	uploadURL, err := attachmentHTTPSURL(endpoint)
	if err != nil {
		return "", errors.New("invalid attachment upload endpoint")
	}
	sourceURL, err := attachmentHTTPSURL(source)
	if err != nil || (sourceURL.Hostname() != "cdn.discordapp.com" && sourceURL.Hostname() != "media.discordapp.net") ||
		(sourceURL.Port() != "" && sourceURL.Port() != "443") || !discordAttachmentPath.MatchString(sourceURL.Path) {
		return "", errors.New("invalid Discord attachment URL")
	}
	filename := path.Base(sourceURL.Path)
	if filename == "." || filename == ".." || strings.IndexFunc(filename, unicode.IsControl) >= 0 {
		return "", errors.New("invalid Discord attachment filename")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL.String(), nil)
	if err != nil {
		return "", errors.New("invalid attachment download request")
	}
	download.Header.Set("Accept-Encoding", "identity")
	downloaded, err := u.downloadClient.Do(download)
	if err != nil {
		return "", errors.New("attachment download failed")
	}
	defer downloaded.Body.Close()
	if downloaded.StatusCode != http.StatusOK {
		return "", errors.New("attachment download was not accepted")
	}
	if downloaded.ContentLength <= 0 || downloaded.ContentLength > maxAttachmentSize {
		return "", errors.New("attachment size is unknown, empty, or exceeds 100 MiB")
	}
	for _, encoding := range downloaded.Header.Values("Content-Encoding") {
		if encoding != "" && !strings.EqualFold(strings.TrimSpace(encoding), "identity") {
			return "", errors.New("unsupported attachment content encoding")
		}
	}
	contentType := downloaded.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return "", errors.New("invalid attachment content type")
	}

	upload, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), io.LimitReader(downloaded.Body, downloaded.ContentLength))
	if err != nil {
		return "", errors.New("invalid attachment upload request")
	}
	upload.ContentLength = downloaded.ContentLength
	upload.Header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	upload.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	upload.SetBasicAuth(u.account, u.password)
	// A filehost can reply before the streamed request has finished writing.
	written := make(chan error, 1)
	upload = upload.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { written <- info.Err },
	}))
	uploaded, err := u.uploadClient.Do(upload)
	if err != nil {
		return "", errors.New("attachment upload failed")
	}
	defer uploaded.Body.Close()
	if uploaded.StatusCode != http.StatusCreated {
		return "", errors.New("attachment upload was not accepted")
	}
	location := uploaded.Header.Get("Location")
	if location == "" || invalidAttachmentURLText(location) {
		return "", errors.New("invalid attachment upload location")
	}
	reference, err := url.Parse(location)
	if err != nil || reference.User != nil || (strings.HasPrefix(location, "//") && reference.Hostname() == "") {
		return "", errors.New("invalid attachment upload location")
	}
	result := uploadURL.ResolveReference(reference).String()
	if _, err := attachmentHTTPSURL(result); err != nil || len(result) > maxAttachmentURLLength {
		return "", errors.New("invalid attachment upload location")
	}
	select {
	case err := <-written:
		if err != nil {
			return "", errors.New("attachment upload failed")
		}
	case <-ctx.Done():
		return "", errors.New("attachment upload timed out")
	}
	return result, nil
}

func attachmentHTTPSURL(raw string) (*url.URL, error) {
	if invalidAttachmentURLText(raw) {
		return nil, errors.New("invalid HTTPS URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid HTTPS URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid HTTPS URL")
		}
	}
	return u, nil
}

func motdVoiceUploadURL(text string) (string, bool) {
	match := motdVoiceFallback.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	parts := strings.Split(match[1], ":")
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || index > 0 && value > 59 {
			return "", false
		}
	}
	if match[3] != "" {
		if _, err := time.Parse(time.RFC3339, match[3]); err != nil {
			return "", false
		}
	}
	voiceURL, err := url.Parse(match[4])
	if err != nil {
		return "", false
	}
	if voiceURL.Fragment != "" {
		key, waveform, found := strings.Cut(voiceURL.Fragment, "=")
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(waveform)
		if !found || key != "motd-wave" || decodeErr != nil || len(decoded) == 0 {
			return "", false
		}
		voiceURL.Fragment = ""
		voiceURL.RawFragment = ""
	}
	return voiceURL.String(), true
}

func sameHTTPSOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || a.Scheme != "https" || b.Scheme != "https" ||
		!strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if value := u.Port(); value != "" {
			return value
		}
		return "443"
	}
	return port(a) == port(b)
}

func invalidAttachmentURLText(raw string) bool {
	return strings.Contains(raw, "#") || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0
}
