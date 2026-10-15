package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

// KeepAliveToken represents a unique token per calendar month.
// This is used to ensure that the generated output changes at least monthly,
// so scheduled workflows keep producing repo activity even when upstream data
// doesn't change.
type KeepAliveToken struct{}

func (g KeepAliveToken) MarshalJSON() ([]byte, error) { return json.Marshal(g.String()) }

func (g *KeepAliveToken) UnmarshalJSON([]byte) error { return nil }

func (KeepAliveToken) String() string {
	return fmt.Sprintf("%x", sha1.Sum([]byte{byte(time.Now().Month())}))
}

type FormsInfo struct {
	// We need this to keep scheduled actions from being disabled due to inactivity
	// when the upstream Standard Forms version doesn't change for a while.
	GhKeepAlive KeepAliveToken `json:"_gh_keepalive"`

	Version    string    `json:"version"`
	ArchiveURL string    `json:"archive_url"`
	Generated  time.Time `json:"-"`
}

func (f FormsInfo) String() string {
	return fmt.Sprintf("version: '%s', url: '%s'", f.Version, f.ArchiveURL)
}

const DirectFormsZipURL = "https://downloads.winlink.org/User%20Programs/Standard_Forms.zip"
const MirrorFormsAPIPath = "https://api.getpat.io/v1/forms/standard-templates/"
const PatFormsAPIPath = "https://forms-api.islandmagic.co/v1/forms/standard-templates/"

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

var (
	// Cloudflare challenges non-browser HTTP/2 requests to downloads.winlink.org.
	formsTransport = http.DefaultTransport.(*http.Transport).Clone()
	formsClient    = &http.Client{
		Timeout:   30 * time.Second,
		Transport: formsTransport,
	}
)

func init() {
	formsTransport.ForceAttemptHTTP2 = false
	formsTransport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	formsTransport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
}

func main() {
	latest, err := downloadZipURL(DirectFormsZipURL)
	if err != nil {
		log.Printf("direct download failed (%v); falling back to api.getpat.io mirror", err)
		mirrorURL, err := getMirrorArchiveURL()
		if err != nil {
			log.Fatalf("could not get latest forms info from mirror: %v", err)
		}
		log.Printf("Found mirror URL %s", mirrorURL)
		latest, err = downloadZipURL(mirrorURL)
		if err != nil {
			log.Fatalf("could not download mirror archive url: %v", err)
		}
		log.Printf("Downloaded from mirror URL %s", mirrorURL)
	} else {
		log.Printf("Downloaded from direct URL %s", DirectFormsZipURL)
	}
	log.Printf("Found version %s", latest.Version)
	_ = json.NewEncoder(os.Stdout).Encode(latest)
}

func getMirrorArchiveURL() (string, error) {
	req, err := http.NewRequest(http.MethodGet, MirrorFormsAPIPath+"latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := formsClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var info FormsInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode mirror latest: %w", err)
	}
	if info.ArchiveURL == "" {
		return "", fmt.Errorf("mirror latest response missing archive_url")
	}
	return info.ArchiveURL, nil
}

func downloadZipURL(zipURL string) (*FormsInfo, error) {
	req, err := http.NewRequest(http.MethodGet, zipURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := formsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusFound, http.StatusOK:
		break
	default:
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	b, version, err := readAndCheckZip(resp.Body)
	if err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("Standard_Forms_%s.zip", version)
	out, err := os.Create(filename)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	_, err = io.Copy(out, bytes.NewReader(b))
	return &FormsInfo{
		Version:    version,
		ArchiveURL: PatFormsAPIPath + filename,
		Generated:  time.Now(),
	}, err
}

func readAndCheckZip(rc io.ReadCloser) ([]byte, string, error) {
	var version string
	// https://stackoverflow.com/a/50539327/587091
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", err
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, "", err
	}

	// Read all the files from zip archive
	for _, zipFile := range zipReader.File {
		// If the file is Standard_Forms_Version.dat, read the contents
		if zipFile.Name == "Standard_Forms_Version.dat" {
			ver, err := readZipFileContents(zipFile)
			if err != nil {
				return nil, "", err
			}
			// strings.TrimSpace is not sufficient. Version 1.1.6.0 was released as `1.1.6\t.0`
			version = stripSpaces(string(ver))
		}

		// Otherwise, read the file to check for errors but discard the contents
		if err = readZipFile(zipFile); err != nil {
			return nil, "", err
		}
	}

	return body, version, nil
}

func stripSpaces(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, str)
}

func readZipFile(zf *zip.File) error {
	f, err := zf.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(io.Discard, f)
	return err
}

func readZipFileContents(zf *zip.File) ([]byte, error) {
	f, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
