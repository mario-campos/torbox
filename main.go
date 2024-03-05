package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/integrii/flaggy"
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

	subcommandList := flaggy.NewSubcommand("list")
	subcommandList.Bool(&isHumanReadable, "H", "human-readable", "Human-readable output")
	subcommandList.Bool(&isJSON, "J", "json", "JSON output")
	flaggy.AttachSubcommand(subcommandList, 1)

	subcommandDownload := flaggy.NewSubcommand("download")
	flaggy.AttachSubcommand(subcommandDownload, 1)

	flaggy.SetName("torbox")
	flaggy.DefaultParser.DisableShowVersionWithVersion()
	flaggy.Parse()

	if TORBOX_API_KEY == "" {
		slog.Warn("TORBOX_API_KEY environment variable is not set; torbox will likely fail to authenticate with torbox.app.")
	}

	if subcommandList.Used {
		var torboxBody []byte

		if isJSON && isHumanReadable {
			fmt.Fprintln(os.Stderr, "error: cannot use both --json and --human-readable flags")
			os.Exit(1)
		}

		// Request the list of ttl from the torbox.app API.
		client := &http.Client{}
		req, err := http.NewRequest("GET", "https://api.torbox.app/v1/api/torrents/mylist", nil)
		if err != nil {
			slog.Error("failed to create HTTP request object: ", err)
		}
		req.Header.Add("Authorization", "bearer "+TORBOX_API_KEY)
		resp, err := client.Do(req)
		if err != nil {
			slog.Error("HTTP request failed: ", err)
		}
		if resp.StatusCode != http.StatusOK {
			slog.Error("HTTP status (", resp.Status, ") does not match expected status (200 OK)")
		}
		defer resp.Body.Close()
		torboxBody, err = io.ReadAll(resp.Body)
		if err != nil {
			slog.Error("failed to read HTTP response body: ", err)
		}

		if isJSON {
			fmt.Println(string(torboxBody))
		} else {
			if err = json.Unmarshal(torboxBody, &ttl); err != nil {
				slog.Error("failed to decode JSON response: ", err)
			}
			for _, torrent := range ttl.Data {
				if isHumanReadable {
					fmt.Printf("%d %3.f%% %s  %s\n", torrent.ID, torrent.Progress*100, HumanReadableSize(torrent.Size), torrent.Name)
				} else {
					fmt.Printf("%d %.2f %d\t%s\n", torrent.ID, torrent.Progress, torrent.Size, torrent.Name)
				}
			}
		}
		return
	}

	if subcommandDownload.Used {
		var client http.Client

		if err := json.NewDecoder(os.Stdin).Decode(&ttl); err != nil {
			slog.Error("failed to decode JSON input: ", err)
		}

		for _, torrent := range ttl.Data {
			for _, torrentfile := range torrent.Files {
				var downloadRequest TorboxDownloadResponse

				req, err := http.NewRequest("GET", fmt.Sprintf("https://api.torbox.app/v1/api/torrents/requestdl?token=%s&torrent_id=%d&file_id=%d", TORBOX_API_KEY, torrent.ID, torrentfile.ID), nil)
				if err != nil {
					slog.Error("failed to create HTTP request object: ", err)
				}
				resp, err := client.Do(req)
				if err != nil {
					slog.Error("HTTP request failed: ", err)
				}
				if resp.StatusCode != http.StatusOK {
					slog.Error("HTTP status (", resp.Status, ") does not match expected status (200 OK)")
				}
				json.NewDecoder(resp.Body).Decode(&downloadRequest)
				if err = resp.Body.Close(); err != nil {
					slog.Warn("failed to close HTTP response body: ", err)
				}
				req, err = http.NewRequest("GET", downloadRequest.Data, nil)
				if err != nil {
					slog.Error("failed to create HTTP request object: ", err)
				}
				resp, err = client.Do(req)
				if err != nil {
					slog.Error("HTTP request failed: ", err)
				}
				if resp.StatusCode != http.StatusOK {
					slog.Error("HTTP status (", resp.Status, ") does not match expected status (200 OK)")
				}
				if err = resp.Body.Close(); err != nil {
					slog.Warn("failed to close HTTP response body: ", err)
				}

				if err = os.MkdirAll(filepath.Dir(torrentfile.Name), 0755); err != nil {
					slog.Error("failed to create directory '", filepath.Dir(torrentfile.Name), "': ", err)
				}

				cmd := exec.Command("wget", "--directory-prefix", filepath.Dir(torrentfile.Name), "--output-document", torrentfile.Name, downloadRequest.Data)
				cmd.Stdout = os.Stdout // Redirect wget's output and error streams to this program's output and error streams.
				cmd.Stderr = os.Stderr // So that the user sees the progress of the download.
				slog.Info(cmd.String())
				if err = cmd.Run(); err != nil {
					slog.Error("failed to execute download command: ", err)
				}

				// Verify that the file's MD5 hash matches the TorBox API's MD5 hash.
				if f, err := os.Open(torrentfile.Name); err == nil {
					defer f.Close()
					h := md5.New()
					if _, err := io.Copy(h, f); err != nil {
						slog.Error("failed to calculate MD5 hash of '", torrentfile.Name, "': ", err)
						continue
					}
					if torrentfile.MD5 != fmt.Sprintf("%x", h.Sum(nil)) {
						slog.Error("MD5 hash mismatch")
					}
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
