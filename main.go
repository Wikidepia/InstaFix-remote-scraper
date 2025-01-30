package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unsafe"

	_ "net/http/pprof"

	"github.com/kelindar/binary"
	"github.com/kelindar/binary/nocopy"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tidwall/gjson"
)

type Media struct {
	TypeName nocopy.String
	URL      nocopy.String
}

type InstaData struct {
	PostID   nocopy.String
	Username nocopy.String
	Caption  nocopy.String
	Medias   []Media
}

// Copied from DefaultTransport
var reqHeader http.Header
var transportCache *http.Transport
var semaphore = make(chan struct{}, 32)

// b2s converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func init() {
	reqHeader = http.Header{}
	reqHeader.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:128.0) Gecko/20100101 Firefox/128.0")
	reqHeader.Set("Accept", "*/*")
	reqHeader.Set("Accept-Language", "en-US,en;q=0.5")
	reqHeader.Set("Content-Type", "application/x-www-form-urlencoded")
	reqHeader.Set("X-FB-Friendly-Name", "PolarisPostActionLoadPostQueryQuery")
	reqHeader.Set("Origin", "https://www.instagram.com")
	reqHeader.Set("DNT", "1")
	reqHeader.Set("Sec-GPC", "1")
	reqHeader.Set("Connection", "keep-alive")
	reqHeader.Set("Sec-Fetch-Dest", "empty")
	reqHeader.Set("Sec-Fetch-Mode", "cors")
	reqHeader.Set("Sec-Fetch-Site", "same-origin")
	reqHeader.Set("Pragma", "no-cache")
	reqHeader.Set("Cache-Control", "no-cache")
	reqHeader.Set("TE", "trailers")

	baseDialFunc := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}).DialContext
	transportCache = &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	transportCache.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "www.instagram.com:443" {
			// IP is geo based, need to add some flag
			return baseDialFunc(ctx, network, "157.240.7.174:443")
		}
		return baseDialFunc(ctx, network, addr)
	}
}

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	serverAddr := flag.String("server-addr", "", "server address")
	interfaceAddr := flag.String("interface-addr", "", "interface address")
	authCode := flag.String("authcode", "", "auth code")
	flag.Parse()

	if *serverAddr == "" || *authCode == "" {
		log.Fatal().Msg("server-addr or authcode is empty")
		return
	}

	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()

	laddr, err := net.ResolveTCPAddr("tcp", *interfaceAddr)
	if err != nil {
		log.Fatal().Msg("resolve tcp addr error")
		return
	}
	d := net.Dialer{Timeout: 5 * time.Second, LocalAddr: laddr}

	for {
		semaphore <- struct{}{}

		// Get a TCP connection
		conn, err := d.Dial("tcp", *serverAddr)
		if err != nil {
			log.Error().Err(err).Msg("dial error, sleeping 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}

		conn.Write([]byte(*authCode))
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer func() {
		conn.Close()
		<-semaphore
	}()

	for {
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			log.Error().Err(err).Msg("set deadline error")
			return
		}

		buf := make([]byte, 128)
		n, err := conn.Read(buf)
		if err != nil {
			log.Error().Err(err).Msg("read error")
			return
		}

		log.Info().Str("buf", b2s(buf[:n])).Msg("Received data")
		idata, err := handleScrape(b2s(buf[:n]))
		if err != nil {
			log.Error().Str("buf", b2s(buf[:n])).Err(err).Msg("scraping error")
		}

		idataMarshal, err := binary.Marshal(idata)
		if err != nil {
			log.Error().Err(err).Msg("marshal error")
			return
		}

		if _, err := conn.Write(idataMarshal); err != nil {
			log.Error().Err(err).Msg("write error")
			return
		}
	}
}

func handleScrape(postID string) (InstaData, error) {
	var err error
	idata := InstaData{
		PostID: nocopy.String(postID),
	}

	if postID[0] == 'B' {
		postID, err = getSharePostID(postID)
		if err != nil {
			return InstaData{}, err
		}
		idata.PostID = nocopy.String(postID)
	}

	response, err := parseGQL(postID)
	if err != nil {
		return idata, err
	}

	data := gjson.Parse(b2s(response)).Get("data")
	if !bytes.Contains(response, []byte("shortcode_media")) {
		return idata, errors.New("post not found")
	}

	var item gjson.Result
	if bytes.Contains(response, []byte("xdt_shortcode_media")) {
		item = data.Get("xdt_shortcode_media")
	} else {
		item = data.Get("shortcode_media")
	}

	if item.Value() == nil {
		return idata, errors.New("shortcode_media is empty")
	}

	idata.Username = nocopy.String(item.Get("owner.username").String())
	idata.Caption = nocopy.String(item.Get("edge_media_to_caption.edges.0.node.text").String())

	// Get medias
	var media []gjson.Result
	if bytes.Contains(response, []byte("edge_sidecar_to_children")) {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	} else {
		media = []gjson.Result{item}
	}

	idata.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		}
		idata.Medias = append(idata.Medias, Media{
			TypeName: nocopy.String(m.Get("__typename").String()),
			URL:      nocopy.String(mediaURL.String()),
		})
	}

	if len(idata.Username) == 0 {
		return InstaData{}, errors.New("post not found")
	}
	return idata, nil
}

func parseGQL(postID string) ([]byte, error) {
	params := url.Values{
		"variables":         {"{\"shortcode\":\"" + postID + "\",\"fetch_tagged_user_count\":null,\"hoisted_comment_id\":null,\"hoisted_reply_id\":null}"},
		"server_timestamps": {"true"},
		"doc_id":            {"8845758582119845"},
	}

	client := &http.Client{Transport: transportCache, Timeout: 3 * time.Second}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query", strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header = reqHeader

	buf := new(bytes.Buffer)
	var res *http.Response
	// TODO Sometimes api returns 5xx error, retrying doesn't help.
	for i := 0; i < 3; i++ {
		res, err = client.Do(req)
		if err != nil {
			continue
		}
		defer res.Body.Close()
		buf.Reset() // Reset buffer
		if _, err = buf.ReadFrom(res.Body); err != nil {
			continue
		}
		if bytes.Contains(buf.Bytes(), []byte("require_login")) {
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func getSharePostID(postID string) (string, error) {
	req, err := http.NewRequest("HEAD", "https://www.instagram.com/share/p/"+postID+"/", nil)
	if err != nil {
		return postID, err
	}
	resp, err := transportCache.RoundTrip(req)
	if err != nil {
		return postID, err
	}
	defer resp.Body.Close()
	redirURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return postID, err
	}
	postIDTemp := path.Base(redirURL.Path)
	if postIDTemp == "login" {
		return postID, errors.New("not logged in")
	}
	return postIDTemp, nil
}
