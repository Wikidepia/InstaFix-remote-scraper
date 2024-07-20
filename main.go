package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"net"
	"net/http"
	"strings"
	"time"
	"unsafe"

	"github.com/CAFxX/httpcompression"
	"github.com/CAFxX/httpcompression/contrib/klauspost/zstd"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/kelindar/binary"
	"github.com/kelindar/binary/nocopy"
	"github.com/klauspost/compress/gzhttp"
	kpzstd "github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"go.mercari.io/go-dnscache"
	"go.uber.org/ratelimit"
	"golang.org/x/exp/rand"
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
var transport http.RoundTripper
var header = http.Header{
	"accept":                      {"*/*"},
	"accept-language":             {"en-US,en;q=0.9"},
	"content-type":                {"application/x-www-form-urlencoded"},
	"origin":                      {"https://www.instagram.com"},
	"priority":                    {"u=1, i"},
	"sec-ch-prefers-color-scheme": {"dark"},
	"sec-ch-ua":                   {`"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`},
	"sec-ch-ua-full-version-list": {`"Google Chrome";v="125.0.6422.142", "Chromium";v="125.0.6422.142", "Not.A/Brand";v="24.0.0.0"`},
	"sec-ch-ua-mobile":            {"?0"},
	"sec-ch-ua-model":             {`""`},
	"sec-ch-ua-platform":          {`"macOS"`},
	"sec-ch-ua-platform-version":  {`"12.7.4"`},
	"sec-fetch-dest":              {"empty"},
	"sec-fetch-mode":              {"cors"},
	"sec-fetch-site":              {"same-origin"},
	"user-agent":                  {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"},
	"x-asbd-id":                   {"129477"},
	"x-bloks-version-id":          {"e2004666934296f275a5c6b2c9477b63c80977c7cc0fd4b9867cb37e36092b68"},
	"x-fb-friendly-name":          {"PolarisPostActionLoadPostQueryQuery"},
	"x-ig-app-id":                 {"936619743392459"},
}
var postData = "__a=1&__ccg=UNKNOWN&__comet_req=7&__csr=n2Yfg_5hcQAG5mPtfEzil8Wn-DpKGBXhdczlAhrK8uHBAGuKCJeCieLDyExenh68aQAKta8p8ShogKkF5yaUBqCpF9XHmmhoBXyBKbQp0HCwDjqoOepV8Tzk8xeXqAGFTVoCciGaCgvGUtVU-u5Vp801nrEkO0rC58xw41g0VW07ISyie2W1v7F0CwYwwwvEkw8K5cM0VC1dwdi0hCbc094w6MU1xE02lzw&__d=www&__dyn=7xeUjG1mxu1syUbFp40NonwgU7SbzEdF8aUco2qwJw5ux609vCwjE1xoswaq0yE6ucw5Mx62G5UswoEcE7O2l0Fwqo31w9a9wtUd8-U2zxe2GewGw9a362W2K0zK5o4q3y1Sx-0iS2Sq2-azo7u3C2u2J0bS1LwTwKG1pg2fwxyo6O1FwlEcUed6goK2O4UrAwCAxW6Uf9EObzVU8U&__hs=19888.HYP%3Ainstagram_web_pkg.2.1..0.0&__hsi=7380500578385702299&__req=k&__rev=1014227545&__s=trbjos%3An8dn55%3Ayev1rm&__spin_b=trunk&__spin_r=1014227545&__spin_t=1718406700&__user=0&av=0&doc_id=25531498899829322&dpr=2&fb_api_caller_class=RelayModern&fb_api_req_friendly_name=PolarisPostActionLoadPostQueryQuery&jazoest=2882&lsd=AVoPBTXMX0Y&server_timestamps=true&variables=%7B%22shortcode%22%3A%22$$POSTID$$%22%7D"
var rl = ratelimit.New(20)

//go:embed dictionary.bin
var dict []byte

// b2s converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func main() {
	resolver, err := dnscache.New(5*time.Minute, 5*time.Second)
	if err != nil {
		panic(err)
	}
	rand.Seed(uint64(time.Now().UTC().UnixNano()))

	transportCache := &http.Transport{
		// ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	cacheDialCtx := dnscache.DialFunc(resolver, nil)
	baseDialFunc := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}).DialContext
	transportCache.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "www.instagram.com:443" {
			// IP is geo based, need to add some flag
			return baseDialFunc(ctx, network, "157.240.218.174:443")
		}
		return cacheDialCtx(ctx, network, addr)
	}
	transport = gzhttp.Transport(transportCache, gzhttp.TransportAlwaysDecompress(true))

	r := chi.NewRouter()
	zdEnc, err := zstd.New(kpzstd.WithEncoderDict(dict), kpzstd.WithEncoderLevel(kpzstd.SpeedFastest))
	if err != nil {
		panic(err)
	}
	compressor, err := httpcompression.Adapter(
		httpcompression.Compressor("zstd.dict", 1, zdEnc),
	)
	if err != nil {
		panic(err)
	}
	r.Use(middleware.Logger)
	r.Use(compressor)
	r.Mount("/debug", middleware.Profiler())
	r.Get("/scrape/{postID}", http.HandlerFunc(Scrape))

	err = http.ListenAndServe(":3001", r)
	if err != nil {
		panic(err)
	}
}

func Scrape(w http.ResponseWriter, r *http.Request) {
	rl.Take()
	postID := chi.URLParam(r, "postID")

	var i InstaData

	// 1. Use Embed
	// 2. Scrape from graphql
	response, err := ParseGQL(postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := gjson.Parse(b2s(response)).Get("data")

	if !bytes.Contains(response, []byte("shortcode_media")) {
		http.Error(w, "Post not found", http.StatusNotFound)
		return
	}

	var item gjson.Result
	if bytes.Contains(response, []byte("xdt_shortcode_media")) {
		item = data.Get("xdt_shortcode_media")
	} else {
		item = data.Get("shortcode_media")
	}

	i.PostID = nocopy.String(postID)

	// Get username
	i.Username = nocopy.String(item.Get("owner.username").String())

	// Get caption
	i.Caption = nocopy.String(item.Get("edge_media_to_caption.edges.0.node.text").String())

	// Get medias
	var media []gjson.Result
	if bytes.Contains(response, []byte("edge_sidecar_to_children")) {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	} else {
		media = []gjson.Result{item}
	}

	i.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		}
		i.Medias = append(i.Medias, Media{
			TypeName: nocopy.String(m.Get("__typename").String()),
			URL:      nocopy.String(mediaURL.String()),
		})
	}

	err = binary.MarshalTo(i, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func ParseGQL(postID string) ([]byte, error) {
	newParams := strings.Replace(postData, "$$POSTID$$", postID, -1)
	client := http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query", strings.NewReader(newParams))
	if err != nil {
		return nil, err
	}

	req.Header = header

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
