/*
 * Copyright (c) 2023 Savio Sena <savio.sena@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
 * THE SOFTWARE.
 */

package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	_ "embed"

	"github.com/fhs/gompd/mpd"
	"github.com/spf13/viper"
)

const ConfigFile = ".mpd-brainz.conf"

const listenBrainzURL = "https://api.listenbrainz.org/1/submit-listens"

var (
	lastListen   Listens
	verbose      bool
	printVersion bool
	importShazam string

	mpdAddress  string
	mpdPassword string
	interval    time.Duration
	token       string
)

//go:embed VERSION
var Version string

func PrintVersion() {
	fmt.Printf("mpd-brainz v%s", Version)
	os.Exit(0)
}

func Log(fmt string, args ...any) {
	log.Printf(fmt+"\n", args...)
}

func Debug(fmt string, args ...any) {
	if verbose {
		log.Printf(fmt+"\n", args...)
	}
}

func Error(fmt string, args ...any) {
	log.Printf("error: "+fmt+"\n", args...)
}

func Fatal(fmt string, args ...any) {
	log.Fatalf("error: "+fmt+"\n", args...)
}

type Info struct {
	MediaPlayer             string   `json:"media_player,omitempty"`
	MusicService            string   `json:"music_service,omitempty"`
	MusicServiceName        string   `json:"music_service_name,omitempty"`
	OriginUrl               string   `json:"origin_url,omitempty"`
	SubmissionClient        string   `json:"submission_client,omitempty"`
	SubmissionClientVersion string   `json:"submission_client_version,omitempty"`
	Tags                    []string `json:"tags,omitempty"`
	Duration                int      `json:"duration,omitempty"`
}

type Track struct {
	Info        Info   `json:"additional_info,omitempty"`
	ArtistName  string `json:"artist_name,omitempty"`
	TrackName   string `json:"track_name,omitempty"`
	ReleaseName string `json:"release_name,omitempty"`
}

type Listen struct {
	ListenedAt int64 `json:"listened_at,omitempty"`
	Track      Track `json:"track_metadata,omitempty"`
}

func (l *Listen) String() string {
	return fmt.Sprintf("\"%s - %s\"", l.Track.ArtistName, l.Track.TrackName)
}

type Listens struct {
	ListenType string   `json:"listen_type,omitempty"`
	Payload    []Listen `json:"payload,omitempty"`
}

const ListensMaxSize = 500

func NewListens(listenType string) Listens {
	return Listens{
		ListenType: listenType,
		Payload:    []Listen{},
	}
}

func NewListen(listenType string, artistName string, trackName string,
	releaseName string, originUrl string, musicService string, timestamp int64) Listens {
	listens := NewListens("single")
	listens.Add(artistName, trackName, releaseName, originUrl, musicService, timestamp)
	return listens
}

func (l *Listens) Length() int {
	return len(l.Payload)
}

func (l *Listens) String() string {
	s := ""
	n := l.Length()
	if n == 1 {
		return l.Payload[0].String()
	}
	for i := 0; i < n; i++ {
		t := l.Payload[i].String()
		if i != n-1 {
			t += ", "
		}
		s += t
	}
	return fmt.Sprintf("{%s, [%s]}", l.ListenType, s)
}

func (l *Listens) IsNil() bool {
	return l == nil ||
		l.Length() == 0 ||
		l.Payload[0].Track.ArtistName == "" ||
		l.Payload[0].Track.TrackName == ""
}

func (l *Listens) Equal(o Listens) bool {
	return l != nil && l.Length() > 0 && o.Length() > 0 &&
		l.Payload[0].Track.ArtistName == o.Payload[0].Track.ArtistName &&
		l.Payload[0].Track.TrackName == o.Payload[0].Track.TrackName
}

func (l *Listens) Add(artistName string, trackName string, releaseName string,
	originUrl string, musicService string, listenedAt int64) {
	if listenedAt == 0 {
		listenedAt = time.Now().Unix()
	}

	// When receiving metadata in a unified field, particularly during online
	// radio playback, we attempt to parse and interpret it based on our
	// discoveries. As there isn't a set standard to ascertain the sequence,
	// the order we establish is essentially an inference from the data
	// received from these online sources. If inconsistencies arise with the
	// established orders, it might be necessary to allow proper customization
	// in the configuration file.

	if artistName == "" && strings.Contains(trackName, " - ") {
		elems := strings.Split(trackName, " - ")
		n := len(elems)
		switch n {
		case 2:
			artistName = elems[0]
			trackName = elems[1]
		case 4:
			fallthrough
		case 3:
			trackName = elems[0]
			artistName = elems[1]
			releaseName = elems[2]
		}
	}

	l.Payload = append(l.Payload, Listen{
		ListenedAt: listenedAt,
		Track: Track{
			ArtistName:  artistName,
			TrackName:   trackName,
			ReleaseName: releaseName,
			Info: Info{
				SubmissionClient:        "mpd-brainz",
				SubmissionClientVersion: Version,
				MusicService:            musicService,
				OriginUrl:               originUrl,
			},
		},
	})
}

func (l *Listens) Submit(listenType string, token string) error {
	jsonData, err := json.MarshalIndent(l, "", "   ")
	if err != nil {
		return err
	}

	l.ListenType = listenType

	if l.ListenType == "playing_now" {
		l.Payload[0].ListenedAt = 0
	} else if l.ListenType == "import" {
		Log("importing %d listens", l.Length())
	} else {
		Log("submitting listen: %s", l)
	}

	req, err := http.NewRequest("POST", listenBrainzURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		Debug("bad request with data: %s", jsonData)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error submitting request. status: %s", resp.Status)
	}

	return nil
}

func getCurrentListen(conn *mpd.Client) (Listens, error) {
	currentSong, err := conn.CurrentSong()
	if err != nil {
		return Listens{}, err
	}

	artistName := currentSong["Artist"]
	trackName := currentSong["Title"]
	releaseName := currentSong["Album"]
	originUrl := currentSong["file"]
	musicService := currentSong["Name"]

	return NewListen("single", artistName, trackName, releaseName,
		originUrl, musicService, 0), nil
}

func scrobble() {
	conn, err := mpd.DialAuthenticated("tcp", mpdAddress, mpdPassword)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	Log("connected to MPD: %s", mpdAddress)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	Debug("scrobbling with an interval: %s", interval)

	for {
		select {
		case <-ticker.C:
			currentListen, err := getCurrentListen(conn)
			if err != nil {
				log.Println("error obtaining current song from MPD:", err)
				continue
			}
			if !currentListen.Equal(lastListen) && !currentListen.IsNil() {
				err = currentListen.Submit("single", token)
				if err != nil {
					Error("submitting scrobble to ListenBrainz: %s", err)
					continue
				}
				err = currentListen.Submit("playing_now", token)
				if err != nil {
					Error("submitting \"playing now\" to ListenBrainz: %s", err)
					continue
				}
				lastListen = currentListen
			} else {
			}
		case <-stop:
			return
		}
	}
}

func skipLine(file *os.File) {
	info, err := file.Stat()
	if err != nil {
		Fatal("reading file stats: %s: %s", file.Name, err)
	}

	var n int = int(info.Size())
	var b []byte = []byte{' '}

	for i := 0; i < n; i++ {
		_, err = file.Read(b)
		if b[0] == '\n' {
			break
		}
	}
}

func dateToUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		Error("parsing date: %s: %s", date, err)
		return 0
	}
	return t.Unix()
}

func shazamBuffListens(reader *csv.Reader, listen *Listens) bool {
	for i := 0; i < ListensMaxSize; i++ {
		e, err := reader.Read()
		if err != nil {
			if err.Error() == "EOF" {
				return true
			}
			Error("%s", err)
			i -= 1
			continue
		}
		listen.Add(e[3], e[2], "", e[4], "shazam.com", dateToUnix(e[1]))
	}
	return false
}

func shazam() {
	file, err := os.Open(importShazam)
	if err != nil {
		Fatal("opening file: %s", err)
	}
	defer file.Close()

	skipLine(file)
	skipLine(file)

	reader := csv.NewReader(file)
	for {
		listens := NewListens("import")
		finished := shazamBuffListens(reader, &listens)
		err = listens.Submit("import", token)
		if err != nil {
			Fatal("submitting \"import\" to ListenBrainz: %s", err)
		}
		if finished {
			break
		}
	}
}

func config() {
	viper.SetConfigName(ConfigFile)
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME")

	viper.SetDefault("mpd_address", "localhost:6600")
	viper.SetDefault("polling_interval_seconds", 10)
	viper.SetDefault("listenbrainz_token", "")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			log.Fatalf("Error opening configuration file: %s", err)
		}
	}

	mpdAddress = viper.GetString("mpd_address")
	mpdPassword = viper.GetString("mpd_password")
	interval = viper.GetDuration("polling_interval_seconds") * time.Second
	token = viper.GetString("listenbrainz_token")
	if token == "" {
		token = os.Getenv("LISTENBRAINZ_TOKEN")
	}
	if token == "" {
		log.Fatal(fmt.Sprintln("ListenBrainz token not found.",
			"Either define LISTENBRAINZ_TOKEN or set listenbrainz_token in",
			"~/"+ConfigFile+"."))
	}
}

func optarg() {
	flag.BoolVar(&verbose, "v", false, "Enable debug logs.")
	flag.BoolVar(&printVersion, "V", false, "Print version number.")
	flag.StringVar(&importShazam, "i", "", "Import Shazam Library.")
	flag.Parse()

	if printVersion {
		PrintVersion()
	}
}

func main() {
	optarg()
	config()

	if importShazam != "" {
		shazam()
	} else {
		scrobble()
	}
}
