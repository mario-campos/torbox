package main

import (
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

	subcommandList := flaggy.NewSubcommand("list")
	subcommandList.Bool(&isHumanReadable, "H", "human-readable", "Human-readable output")
	subcommandList.Bool(&isJSON, "J", "json", "JSON output")
	flaggy.AttachSubcommand(subcommandList, 1)

	subcommandDownload := flaggy.NewSubcommand("download")
	subcommandDownload.Bool(&isNoDownload, "D", "no-download", "Output the download URLs to standard output, but do not download the files")
	subcommandDownload.Bool(&isNulSep, "0", "null", "Use the ASCII NUL character (0x00) as the delimiter between filenames; implies --no-download.")
	flaggy.AttachSubcommand(subcommandDownload, 1)

	subcommandMD5sum := flaggy.NewSubcommand("md5sum")
	flaggy.AttachSubcommand(subcommandMD5sum, 1)

	flaggy.SetName("torbox")
	flaggy.DefaultParser.DisableShowVersionWithVersion()
	flaggy.Parse()

	if TORBOX_API_KEY == "" {
		Warn("TORBOX_API_KEY environment variable is not set; torbox will likely fail to authenticate with torbox.app.")
	}

	if subcommandList.Used {
		var torboxBody []byte

		if isJSON && isHumanReadable {
			Error("cannot use both --json and --human-readable flags")
		}

		// Request the list of ttl from the torbox.app API.
		client := &http.Client{}
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

		if isJSON {
			fmt.Println(string(torboxBody))
		} else {
			if err = json.Unmarshal(torboxBody, &ttl); err != nil {
				Error("failed to decode JSON response: %s", err)
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

		// -0,--null implies -D,--no-download.
		if isNulSep {
			isNoDownload = true
		}

		if err := json.NewDecoder(os.Stdin).Decode(&ttl); err != nil {
			Error("failed to decode standard input as JSON: %s", err)
		}

		for _, torrent := range ttl.Data {
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
			}
		}
		return
	}

	if subcommandMD5sum.Used {
		if err := json.NewDecoder(os.Stdin).Decode(&ttl); err != nil {
			Error("failed to decode standard input as JSON: %s", err)
		}
		for _, torrent := range ttl.Data {
			for _, torrentfile := range torrent.Files {
				// This format is compatible with md5sum(1):
				//    <md5sum>  <filename>
				fmt.Printf("%s  %s\n", torrentfile.MD5, torrentfile.Name)
			}
		}
		return
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
