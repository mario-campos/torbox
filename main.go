package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/integrii/flaggy"
	"github.com/ryanuber/go-glob"
)

type TorboxTorrentList struct {
	Detail string
	Data   []TorboxTorrent
}

type TorboxTorrent struct {
	ID               int64
	Hash             string
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Name             string
	Size             int64
	Active           bool
	DownloadState    string `json:"download_state"`
	DownloadFinished bool   `json:"download_finished"`
	Progress         float64
	Files            []TorboxTorrentFile
}

type TorboxTorrentFile struct {
	ID        int
	MD5       string
	Name      string
	Size      int64
	MimeType  string
	ShortName string `json:"short_name"`
}

type TorboxDownloadResponse struct {
	Detail string
	Data   string
}

var TORBOX_API_KEY = os.Getenv("TORBOX_API_KEY")

func main() {
	var isJSON bool
	var isHumanReadable bool
	var ttl TorboxTorrentList
	var torboxBody []byte
	var client http.Client
	var torrentNameHint string

	subcommandList := flaggy.NewSubcommand("list")
	subcommandList.Bool(&isHumanReadable, "H", "human-readable", "Human-readable output")
	subcommandList.Bool(&isJSON, "J", "json", "JSON output")
	flaggy.AttachSubcommand(subcommandList, 1)

	subcommandDownload := flaggy.NewSubcommand("download")
	subcommandDownload.AddPositionalValue(&torrentNameHint, "NAME", 1, false, "The name of the torrent to download")
	flaggy.AttachSubcommand(subcommandDownload, 1)

	flaggy.SetName("torbox")
	flaggy.DefaultParser.DisableShowVersionWithVersion()
	flaggy.Parse()

	if TORBOX_API_KEY == "" {
		Warn("TORBOX_API_KEY environment variable is not set; torbox will likely fail to authenticate with torbox.app.")
	}

	if isJSON && isHumanReadable {
		Error("cannot use both --json and --human-readable flags")
	}

	// Request the list of ttl from the torbox.app API.
	req, err := http.NewRequest("GET", "https://api.torbox.app/v1/api/torrents/mylist", nil)
	if err != nil {
		Error("failed to create HTTP request object: %s", err)
	}
	req.Header.Add("Authorization", "bearer "+TORBOX_API_KEY)
	resp, err := client.Do(req)
	if err != nil {
		Error("HTTP request failed: %s", err)
	}
	if resp.StatusCode != http.StatusOK {
		Error("expected HTTP status 200, got: %s", resp.Status)
	}
	defer resp.Body.Close()
	torboxBody, err = io.ReadAll(resp.Body)
	if err != nil {
		Error("failed to read HTTP response body: %s", err)
	}

	if err = json.Unmarshal(torboxBody, &ttl); err != nil {
		Error("failed to decode JSON response: %s", err)
	}

	if subcommandList.Used {
		if isJSON {
			fmt.Println(string(torboxBody))
		} else {
			for _, torrent := range ttl.Data {
				if isHumanReadable {
					fmt.Printf("%d %d%% %s  %s\n", torrent.ID, int(torrent.Progress*100), HumanReadableSize(torrent.Size), torrent.Name)
				} else {
					// This little type casting hack is necessary because otherwise %.2f will round the
					// float up to the nearest number of that precision, which can lead to confusing
					// results, like torrents with 99% progress appearing to be 100% complete.
					fmt.Printf("%d %.2f %d\t%s\n", torrent.ID, float64(int(torrent.Progress*100))/100, torrent.Size, torrent.Name)
				}
			}
		}
		return
	}

	for _, torrent := range ttl.Data {
		for _, torrentfile := range torrent.Files {
			if torrentNameHint == "" || torrentNameHint == torrent.Name || glob.Glob(torrentNameHint, torrentfile.Name) {
				var downloadRequest TorboxDownloadResponse

				if stat, err := os.Stat(torrentfile.Name); err == nil && stat.Size() == torrentfile.Size {
					Info("%s: file already exists", torrentfile.Name)
					if torrentfile.MD5 == "" {
						// The file already exists and it has the expected size. Unfortunately, we cannot
						// verify the MD5 hash because it wasn't provided to us by TorBox, so let's assume
						// it's the same file we would download, and skip to the next one.
						continue
					} else {
						f, err := os.Open(torrentfile.Name)
						if err == nil {
							hash := md5.New()
							if _, err = io.Copy(hash, f); err == nil {
								if fmt.Sprintf("%x", hash.Sum(nil)) == torrentfile.MD5 {
									Info("%s: MD5 OK", torrentfile.Name)
									continue
								} else {
									Warn("%s: MD5 FAILED (expected %s; got %s)", torrentfile.Name, torrentfile.MD5, fmt.Sprintf("%x", hash.Sum(nil)))
								}
							}
						}
					}
				}

				req, err := http.NewRequest("GET", fmt.Sprintf("https://api.torbox.app/v1/api/torrents/requestdl?token=%s&torrent_id=%d&file_id=%d&zip=false", TORBOX_API_KEY, torrent.ID, torrentfile.ID), nil)
				if err != nil {
					Error("failed to create HTTP request object: %s", err)
				}
				resp, err := client.Do(req)
				if err != nil {
					Error("HTTP request failed: %s", err)
				}
				if resp.StatusCode != http.StatusOK {
					Error("expected HTTP status 200, got: %s", resp.Status)
				}
				json.NewDecoder(resp.Body).Decode(&downloadRequest)
				if err = resp.Body.Close(); err != nil {
					Warn("failed to close HTTP response body: %s", err)
				}
				req, err = http.NewRequest("GET", downloadRequest.Data, nil)
				if err != nil {
					Error("failed to create HTTP request object: %s", err)
				}
				resp, err = client.Do(req)
				if err != nil {
					Error("HTTP request failed: %s", err)
				}
				if resp.StatusCode != http.StatusOK {
					Error("expected HTTP status 200, got: %s", resp.Status)
				}
				if err = resp.Body.Close(); err != nil {
					Warn("failed to close HTTP response body: %s", err)
				}

				if err = os.MkdirAll(filepath.Dir(torrentfile.Name), 0755); err != nil {
					Error("failed to create directory '%s': %s", filepath.Dir(torrentfile.Name), err)
				}

				// Download the file.
				out, err := os.OpenFile(torrentfile.Name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err != nil {
					Error("failed to create file '%s': %s", torrentfile.Name, err)
				}
				defer out.Close()

				req, err = http.NewRequest("GET", downloadRequest.Data, nil)
				if err != nil {
					Error("failed to create HTTP request object: %s", err)
				}

				// The torbox.app service is not five 9s reliable. Sometimes, it can
				// take a while for a connection to "succeed." Retry up to 10 times.
				for i := 0; i < 10; i++ {
					Info("Attempting to download '%s'... (#%d)", torrentfile.Name, i+1)
					resp, err = client.Do(req)
					if err != nil {
						Error("HTTP request failed: %s", err)
					}
					if resp.StatusCode == http.StatusOK {
						break
					}
					Warn("expected HTTP status 200, got: %s", resp.Status)
					time.Sleep((1 << i) * time.Second)
				}

				Info("Downloading '%s'...", torrentfile.Name)
				hash := md5.New()
				buffer := make([]byte, 64<<10) // 64 KiB
				for {
					n, err := resp.Body.Read(buffer)
					if err != nil && err != io.EOF {
						Error("failed to read from HTTP response body: %s", err)
					}
					if n == 0 {
						break
					}
					if _, err = out.Write(buffer[:n]); err != nil {
						Error("failed to write to file '%s': %s", torrentfile.Name, err)
					}
					if _, err = hash.Write(buffer[:n]); err != nil {
						Error("failed to generate an MD5 hash of the download: %s", err)
					}
				}

				Info("Downloaded '%s' (%s)", torrentfile.Name, HumanReadableSize(torrentfile.Size))

				if fmt.Sprintf("%x", hash.Sum(nil)) == torrentfile.MD5 {
					Info("%s: MD5 OK", torrentfile.Name)
				} else {
					Warn("%s: MD5 FAILED (expected %s; got %s)", torrentfile.Name, torrentfile.MD5, fmt.Sprintf("%x", hash.Sum(nil)))
					continue
				}
			}
		}
	}
}

// HumanReadableSize takes a file size as an integer value and returns a string
// representation of the size in human-readable format (e.g. 1.5 MiB, 2.3 GiB).
func HumanReadableSize(size int64) string {
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if size < 1024 {
			return fmt.Sprintf("%3.d %s", size, unit)
		}
		size /= 1024
	}
	return "?iB"
}

func Info(msg string, args ...any) {
	log.Printf("INFO "+msg, args...)
}

func Warn(msg string, args ...any) {
	log.Printf("WARN "+msg, args...)
}

func Error(msg string, args ...any) {
	log.Fatalf("ERROR "+msg, args...)
}

// Marshell takes a command object and marshals it into a string representation
// that can be executed in a shell without any whitespace confusions.
func Marshell(cmd *exec.Cmd) string {
	for i, arg := range cmd.Args {
		if strings.Contains(arg, "'") {
			cmd.Args[i] = `"` + arg + `"`
		} else if strings.ContainsAny(arg, ` ?[]#$"`) {
			cmd.Args[i] = `'` + arg + `'`
		}
	}
	return strings.Join(cmd.Args, " ")
}
