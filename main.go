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
	var isNoDownload bool
	var isNulSep bool
	var ttl TorboxTorrentList
	var torboxBody []byte
	var client http.Client
	var torrentNameHint string

	subcommandList := flaggy.NewSubcommand("list")
	subcommandList.Bool(&isHumanReadable, "H", "human-readable", "Human-readable output")
	subcommandList.Bool(&isJSON, "J", "json", "JSON output")
	flaggy.AttachSubcommand(subcommandList, 1)

	subcommandDownload := flaggy.NewSubcommand("download")
	subcommandDownload.Bool(&isNoDownload, "D", "no-download", "Output the download URLs to standard output, but do not download the files")
	subcommandDownload.Bool(&isNulSep, "0", "null", "Use the ASCII NUL character (0x00) as the delimiter between filenames; implies --no-download.")
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

	// -0,--null implies -D,--no-download.
	if isNulSep {
		isNoDownload = true
	}

	for _, torrent := range ttl.Data {
		if torrentNameHint == "" || torrentNameHint == torrent.Name || glob.Glob(torrentNameHint, torrent.Name) {
			for _, torrentfile := range torrent.Files {
				var downloadRequest TorboxDownloadResponse

				req, err := http.NewRequest("GET", fmt.Sprintf("https://api.torbox.app/v1/api/torrents/requestdl?token=%s&torrent_id=%d&file_id=%d", TORBOX_API_KEY, torrent.ID, torrentfile.ID), nil)
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

				cmd := exec.Command("wget", "--continue", "--directory-prefix", filepath.Dir(torrentfile.Name), "--output-document", torrentfile.Name, downloadRequest.Data)
				cmd.Stdout = os.Stdout // Redirect wget's output and error streams to this program's output and error streams.
				cmd.Stderr = os.Stderr // So that the user sees the progress of the download.

				if isNoDownload {
					if isNulSep {
						fmt.Printf("%s\x00", Marshell(cmd))
					} else {
						fmt.Println(Marshell(cmd))
					}
				} else {
					Info("executing command: %s", cmd.Args)
					if err = cmd.Run(); err != nil {
						Error("failed to execute command: %s", err)
					}
				}

				// Verify the MD5 hash of the downloaded file.
				downloadedFile, err := os.Open(torrentfile.Name)
				if err != nil {
					Warn("failed to open downloaded file '%s': %s", torrentfile.Name, err)
					continue
				}
				defer downloadedFile.Close()

				hash := md5.New()
				_, err = io.Copy(hash, downloadedFile)
				if err != nil {
					Warn("failed to generate an MD5 hash of the download: %s", err)
					continue
				}

				if fmt.Sprintf("%x", hash.Sum(nil)) != torrentfile.MD5 {
					Warn("MD5 hash (%s) of downloaded file '%s' does not match expected MD5 hash (%s)", fmt.Sprintf("%x", hash.Sum(nil)), torrentfile.Name, torrentfile.MD5)
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
