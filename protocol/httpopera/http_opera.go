package httpopera

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"livego/av"
	"livego/configure"
	"livego/protocol/rtmp"
	"livego/protocol/rtmp/rtmprelay"

	jwtmiddleware "github.com/auth0/go-jwt-middleware"
	"github.com/dgrijalva/jwt-go"
)

type Response struct {
	w      http.ResponseWriter
	Status int         `json:"status"`
	Data   interface{} `json:"data"`
}

func (r *Response) SendJson() (int, error) {
	resp, _ := json.Marshal(r)
	r.w.Header().Set("Content-Type", "application/json")
	r.w.WriteHeader(r.Status)
	return r.w.Write(resp)
}

type Operation struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Stop   bool   `json:"stop"`
}

type OperationChange struct {
	Method    string `json:"method"`
	SourceURL string `json:"source_url"`
	TargetURL string `json:"target_url"`
	Stop      bool   `json:"stop"`
}

type ClientInfo struct {
	url              string
	rtmpRemoteClient *rtmp.Client
	rtmpLocalClient  *rtmp.Client
}

type Server struct {
	handler  av.Handler
	session  map[string]*rtmprelay.RtmpRelay
	rtmpAddr string
}

func NewServer(h av.Handler, rtmpAddr string) *Server {
	return &Server{
		handler:  h,
		session:  make(map[string]*rtmprelay.RtmpRelay),
		rtmpAddr: rtmpAddr,
	}
}

func JWTMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(configure.RtmpServercfg.JWTCfg.Secret) > 0 {
			var algorithm jwt.SigningMethod
			if len(configure.RtmpServercfg.JWTCfg.Algorithm) > 0 {
				algorithm = jwt.GetSigningMethod(configure.RtmpServercfg.JWTCfg.Algorithm)
			}

			if algorithm == nil {
				algorithm = jwt.SigningMethodHS256
			}

			jwtMiddleware := jwtmiddleware.New(jwtmiddleware.Options{
				Extractor: jwtmiddleware.FromFirst(jwtmiddleware.FromAuthHeader, jwtmiddleware.FromParameter("jwt")),
				ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
					return []byte(configure.RtmpServercfg.Secret), nil
				},
				SigningMethod: algorithm,
			})

			jwtMiddleware.HandlerWithNext(w, r, next.ServeHTTP)
			return
		}
		next.ServeHTTP(w, r)

	})
}

func (s *Server) Serve(l net.Listener) error {
	mux := http.NewServeMux()

	mux.Handle("/statics/", http.StripPrefix("/statics/", http.FileServer(http.Dir("statics"))))

	mux.HandleFunc("/control/push", func(w http.ResponseWriter, r *http.Request) {
		s.handlePush(w, r)
	})
	mux.HandleFunc("/control/pull", func(w http.ResponseWriter, r *http.Request) {
		s.handlePull(w, r)
	})
	mux.HandleFunc("/control/get", func(w http.ResponseWriter, r *http.Request) {
		s.handleGet(w, r)
	})
	mux.HandleFunc("/control/reset", func(w http.ResponseWriter, r *http.Request) {
		s.handleReset(w, r)
	})
	mux.HandleFunc("/control/delete", func(w http.ResponseWriter, r *http.Request) {
		s.handleDelete(w, r)
	})
	mux.HandleFunc("/stat/livestat", func(w http.ResponseWriter, r *http.Request) {
		s.GetLiveStatics(w, r)
	})
	http.Serve(l, JWTMiddleware(mux))
	return nil
}

type stream struct {
	Key             string `json:"key"`
	Url             string `json:"url"`
	StreamId        uint32 `json:"stream_id"`
	VideoTotalBytes uint64 `json:"video_total_bytes"`
	VideoSpeed      uint64 `json:"video_speed"`
	AudioTotalBytes uint64 `json:"audio_total_bytes"`
	AudioSpeed      uint64 `json:"audio_speed"`
}

type streams struct {
	Publishers []stream `json:"publishers"`
	Players    []stream `json:"players"`
}

//http://127.0.0.1:8090/stat/livestat
func (server *Server) GetLiveStatics(w http.ResponseWriter, req *http.Request) {
	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}

	defer res.SendJson()

	rtmpStream := server.handler.(*rtmp.RtmpStream)
	if rtmpStream == nil {
		res.Status = 500
		res.Data = "Get rtmp stream information error"
		return
	}

	msgs := new(streams)
	for item := range rtmpStream.GetStreams().IterBuffered() {
		if s, ok := item.Val.(*rtmp.Stream); ok {
			if s.GetReader() != nil {
				switch s.GetReader().(type) {
				case *rtmp.VirReader:
					v := s.GetReader().(*rtmp.VirReader)
					msg := stream{item.Key, v.Info().URL, v.ReadBWInfo.StreamId, v.ReadBWInfo.VideoDatainBytes, v.ReadBWInfo.VideoSpeedInBytesperMS,
						v.ReadBWInfo.AudioDatainBytes, v.ReadBWInfo.AudioSpeedInBytesperMS}
					msgs.Publishers = append(msgs.Publishers, msg)
				}
			}
		}
	}

	for item := range rtmpStream.GetStreams().IterBuffered() {
		ws := item.Val.(*rtmp.Stream).GetWs()
		for s := range ws.IterBuffered() {
			if pw, ok := s.Val.(*rtmp.PackWriterCloser); ok {
				if pw.GetWriter() != nil {
					switch pw.GetWriter().(type) {
					case *rtmp.VirWriter:
						v := pw.GetWriter().(*rtmp.VirWriter)
						msg := stream{item.Key, v.Info().URL, v.WriteBWInfo.StreamId, v.WriteBWInfo.VideoDatainBytes, v.WriteBWInfo.VideoSpeedInBytesperMS,
							v.WriteBWInfo.AudioDatainBytes, v.WriteBWInfo.AudioSpeedInBytesperMS}
						msgs.Players = append(msgs.Players, msg)
					}
				}
			}
		}
	}

	resp, _ := json.Marshal(msgs)
	res.Data = resp
}

//http://127.0.0.1:8090/control/pull?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456
func (s *Server) handlePull(w http.ResponseWriter, req *http.Request) {
	var retString string
	var err error

	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}

	defer res.SendJson()

	if req.ParseForm() != nil {
		res.Status = 400
		res.Data = "url: /control/pull?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456"
		return
	}

	oper := req.Form["oper"]
	app := req.Form["app"]
	name := req.Form["name"]
	url := req.Form["url"]

	log.Printf("control pull: oper=%v, app=%v, name=%v, url=%v", oper, app, name, url)
	if (len(app) <= 0) || (len(name) <= 0) || (len(url) <= 0) {
		res.Status = 400
		res.Data = "control push parameter error, please check them."
		return
	}

	remoteurl := "rtmp://127.0.0.1" + s.rtmpAddr + "/" + app[0] + "/" + name[0]
	localurl := url[0]

	keyString := "pull:" + app[0] + "/" + name[0]
	if oper[0] == "stop" {
		pullRtmprelay, found := s.session[keyString]

		if !found {
			retString = fmt.Sprintf("session key[%s] not exist, please check it again.", keyString)
			res.Status = 400
			res.Data = retString
			return
		}
		log.Printf("rtmprelay stop push %s from %s", remoteurl, localurl)
		pullRtmprelay.Stop()

		delete(s.session, keyString)
		retString = fmt.Sprintf("<h1>push url stop %s ok</h1></br>", url[0])
		res.Status = 400
		res.Data = retString
		log.Printf("pull stop return %s", retString)
	} else {
		pullRtmprelay := rtmprelay.NewRtmpRelay(&localurl, &remoteurl)
		log.Printf("rtmprelay start push %s from %s", remoteurl, localurl)
		err = pullRtmprelay.Start()
		if err != nil {
			retString = fmt.Sprintf("push error=%v", err)
		} else {
			s.session[keyString] = pullRtmprelay
			retString = fmt.Sprintf("<h1>push url start %s ok</h1></br>", url[0])
		}
		res.Status = 400
		res.Data = retString
		log.Printf("pull start return %s", retString)
	}
}

//http://127.0.0.1:8090/control/push?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456
func (s *Server) handlePush(w http.ResponseWriter, req *http.Request) {
	var retString string
	var err error

	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}

	defer res.SendJson()

	if req.ParseForm() != nil {
		res.Data = "url: /control/push?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456"
		return
	}

	oper := req.Form["oper"]
	app := req.Form["app"]
	name := req.Form["name"]
	url := req.Form["url"]

	log.Printf("control push: oper=%v, app=%v, name=%v, url=%v", oper, app, name, url)
	if (len(app) <= 0) || (len(name) <= 0) || (len(url) <= 0) {
		res.Data = "control push parameter error, please check them."
		return
	}

	localurl := "rtmp://127.0.0.1" + s.rtmpAddr + "/" + app[0] + "/" + name[0]
	remoteurl := url[0]

	keyString := "push:" + app[0] + "/" + name[0]
	if oper[0] == "stop" {
		pushRtmprelay, found := s.session[keyString]
		if !found {
			retString = fmt.Sprintf("<h1>session key[%s] not exist, please check it again.</h1>", keyString)
			res.Data = retString
			return
		}
		log.Printf("rtmprelay stop push %s from %s", remoteurl, localurl)
		pushRtmprelay.Stop()

		delete(s.session, keyString)
		retString = fmt.Sprintf("<h1>push url stop %s ok</h1></br>", url[0])
		res.Data = retString
		log.Printf("push stop return %s", retString)
	} else {
		pushRtmprelay := rtmprelay.NewRtmpRelay(&localurl, &remoteurl)
		log.Printf("rtmprelay start push %s from %s", remoteurl, localurl)
		err = pushRtmprelay.Start()
		if err != nil {
			retString = fmt.Sprintf("push error=%v", err)
		} else {
			retString = fmt.Sprintf("<h1>push url start %s ok</h1></br>", url[0])
			s.session[keyString] = pushRtmprelay
		}

		res.Data = retString
		log.Printf("push start return %s", retString)
	}
}

//http://127.0.0.1:8090/control/reset?room=ROOM_NAME
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}
	defer res.SendJson()

	if err := r.ParseForm(); err != nil {
		res.Status = 400
		res.Data = "url: /control/reset?room=ROOM_NAME"
		return
	}
	room := r.Form.Get("room")

	if len(room) == 0 {
		res.Status = 400
		res.Data = "url: /control/get?room=ROOM_NAME"
		return
	}

	msg, err := configure.RoomKeys.SetKey(room)

	if err != nil {
		msg = err.Error()
		res.Status = 400
	}

	res.Data = msg
}

//http://127.0.0.1:8090/control/get?room=ROOM_NAME
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}
	defer res.SendJson()

	if err := r.ParseForm(); err != nil {
		res.Status = 400
		res.Data = "url: /control/get?room=ROOM_NAME"
		return
	}

	room := r.Form.Get("room")

	if len(room) == 0 {
		res.Status = 400
		res.Data = "url: /control/get?room=ROOM_NAME"
		return
	}

	msg, err := configure.RoomKeys.GetKey(room)
	if err != nil {
		msg = err.Error()
		res.Status = 400
	}
	res.Data = msg
}

//http://127.0.0.1:8090/control/delete?room=ROOM_NAME
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	res := &Response{
		w:      w,
		Data:   nil,
		Status: 200,
	}
	defer res.SendJson()

	if err := r.ParseForm(); err != nil {
		res.Status = 400
		res.Data = "url: /control/delete?room=ROOM_NAME"
		return
	}

	room := r.Form.Get("room")

	if len(room) == 0 {
		res.Status = 400
		res.Data = "url: /control/get?room=ROOM_NAME"
		return
	}

	if configure.RoomKeys.DeleteChannel(room) {
		res.Data = "Ok"
		return
	}
	res.Status = 404
	res.Data = "Room not found"
}