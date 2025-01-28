package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kelindar/binary"
	"github.com/kelindar/binary/nocopy"
	"github.com/tidwall/gjson"
	"github.com/xtaci/smux"
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
	serverAddr := flag.String("server-addr", "", "server address")
	interfaceAddr := flag.String("interface-addr", "", "interface address")
	authCode := flag.String("authcode", "", "auth code")
	flag.Parse()

	if *serverAddr == "" || *authCode == "" {
		slog.Error("server-addr or authcode is empty")
		return
	}

	addr, err := net.ResolveTCPAddr("tcp", *serverAddr)
	if err != nil {
		slog.Error("resolve tcp addr error", "err", err)
		return
	}

	laddr, err := net.ResolveTCPAddr("tcp", *interfaceAddr)
	if err != nil {
		slog.Error("resolve tcp addr error", "err", err)
		return
	}

	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	for {
		// Get a TCP connection
		conn, err := net.DialTCP("tcp", laddr, addr)
		if err != nil {
			slog.Error("dial error, sleeping 5 seconds", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		conn.Write([]byte(*authCode))

		session, err := smux.Client(conn, smuxConfig)
		if err != nil {
			slog.Error("smux client error", "err", err)
			continue
		}

		go handleSession(session)

		<-session.CloseChan()
	}
}

func handleSession(session *smux.Session) {
	var (
		count     int32
		semaphore = make(chan struct{}, 32)
		closeChan = make(chan struct{})
	)

	defer session.Close()
	for {
		semaphore <- struct{}{}
		atomic.AddInt32(&count, 1)

		// Close session if failed to open new stream
		select {
		case <-closeChan:
			return
		default:
		}

		go func(session *smux.Session, currentCount int32) {
			defer func() { <-semaphore }()

			stream, err := session.OpenStream()
			if err != nil {
				slog.Error("open stream error", "count", currentCount, "err", err)
				closeChan <- struct{}{}
				return
			}
			defer stream.Close()

			for {
				if err := stream.SetDeadline(time.Now().Add(time.Second * 10)); err != nil {
					slog.Error("set deadline error", "count", currentCount, "err", err)
					return
				}

				buf := make([]byte, 128)
				n, err := stream.Read(buf)
				if err != nil {
					slog.Error("read error", "count", currentCount, "err", err)
					return
				}

				slog.Info("Received data", "count", currentCount, "buf", string(buf[:n]))
				idata, err := handleScrape(string(buf[:n]))
				if err != nil {
					slog.Error("scraping error", "count", currentCount, "buf", string(buf[:n]), "err", err)
				}

				idataMarshal, err := binary.Marshal(idata)
				if err != nil {
					slog.Error("marshal error", "err", err)
					return
				}

				if _, err := stream.Write(idataMarshal); err != nil {
					slog.Error("write error", "count", currentCount, "err", err)
					return
				}
			}
		}(session, count)
	}
}

func handleScrape(postID string) (InstaData, error) {
	idata := InstaData{
		PostID: nocopy.String(postID),
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
