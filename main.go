// Package cmd contains an entrypoint for running an ion-sfu instance.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
	"log"
        
	"github.com/gorilla/websocket"
	sfu "github.com/pion/ion-sfu/pkg"
	"github.com/pion/webrtc/v3"
	"github.com/sourcegraph/jsonrpc2"
	websocketjsonrpc2 "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/spf13/viper"

	//"google.golang.org/appengine"
)

var (
	conf = sfu.Config{}
	file string
	cert string
	key  string
	addr string
)

const (
	portRangeLimit = 100
)

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -c {config file}")
	fmt.Println("      -cert {cert file}")
	fmt.Println("      -key {key file}")
	fmt.Println("      -a {listen addr}")
	fmt.Println("      -h (show help info)")
}

func load() bool {
	_, err := os.Stat(file)
	if err != nil {
		return false
	}

	viper.SetConfigFile(file)
	viper.SetConfigType("toml")

	err = viper.ReadInConfig()
	if err != nil {
		fmt.Printf("config file %s read failed. %v\n", file, err)
		return false
	}
	err = viper.GetViper().Unmarshal(&conf)
	if err != nil {
		fmt.Printf("sfu config file %s loaded failed. %v\n", file, err)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) > 2 {
		fmt.Printf("config file %s loaded failed. range port must be [min,max]\n", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) != 0 && conf.WebRTC.ICEPortRange[1]-conf.WebRTC.ICEPortRange[0] <= portRangeLimit {
		fmt.Printf("config file %s loaded failed. range port must be [min, max] and max - min >= %d\n", file, portRangeLimit)
		return false
	}

	fmt.Printf("config %s load ok!\n", file)
	return true
}

func parse() bool {
	flag.StringVar(&file, "c", "config.toml", "config file")
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "a", ":7000", "address to use")
	help := flag.Bool("h", false, "help info")
	flag.Parse()
	if !load() {
		return false
	}

	if *help {
		showHelp()
		return false
	}
	return true
}

type contextKey struct {
	name string
}
type peerContext struct {
	peer *sfu.WebRTCTransport
}

var peerCtxKey = &contextKey{"peer"}

func forContext(ctx context.Context) *peerContext {
	raw, _ := ctx.Value(peerCtxKey).(*peerContext)
	return raw
}

// RPC defines the json-rpc
type RPC struct {
	sfu *sfu.SFU
}

// NewRPC ...
func NewRPC() *RPC {
	return &RPC{
		sfu: sfu.NewSFU(conf),
	}
}

// Join message sent when initializing a peer connection
type Join struct {
	Sid   string                    `json:"sid"`
	Offer webrtc.SessionDescription `json:"offer"`
}

// Negotiation message sent when renegotiating
type Negotiation struct {
	Desc webrtc.SessionDescription `json:"desc"`
}

// Trickle message sent when renegotiating
type Trickle struct {
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

type StreamInfo struct {
	Uid string `json:"uid"`
	StreamId string `json:"streamId"`
	Sid string `json:"sid"`
}

var userIdToStream map[string](map[string]string) = make(map[string](map[string]string))
var userIdToScreenStream map[string](map[string]string) = make(map[string](map[string]string))
var floorIdToSectionId map[string]uint32 = make(map[string]uint32)
var counter uint32 = 1;

// Handle RPC call
func (r *RPC) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	p := forContext(ctx)

	switch req.Method {
	case "join":
		if p.peer != nil {
			log.Printf("connect: peer already exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("peer already exists")),
			})
			break
		}

		var join Join
		err := json.Unmarshal(*req.Params, &join)
		if err != nil {
			log.Printf("connect: error parsing offer: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		var uintSid uint32;

		if sid, ok := floorIdToSectionId[join.Sid]; ok {
			uintSid = sid;
		} else {
			uintSid = counter;
			floorIdToSectionId[join.Sid] = counter;
			counter += 1;
		}

		peer, err := r.sfu.NewWebRTCTransport(uintSid, join.Offer)

		if err != nil {
			log.Printf("connect: error creating peer: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		log.Printf("peer %s join session %d", peer.ID(), join.Sid)

		err = peer.SetRemoteDescription(join.Offer)
		if err != nil {
			log.Printf("Offer error: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		answer, err := peer.CreateAnswer()
		if err != nil {
			log.Printf("Offer error: answer=%v err=%v", answer, err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		err = peer.SetLocalDescription(answer)
		if err != nil {
			log.Printf("Offer error: answer=%v err=%v", answer, err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		// Notify user of trickle candidates
		peer.OnICECandidate(func(c *webrtc.ICECandidate) {
			log.Printf("Sending ICE candidate")
			if c == nil {
				// Gathering done
				return
			}

			if err := conn.Notify(ctx, "trickle", c.ToJSON()); err != nil {
				log.Printf("error sending trickle %s", err)
			}
		})

		peer.OnNegotiationNeeded(func() {
			log.Printf("on negotiation needed called")
			offer, err := p.peer.CreateOffer()
			if err != nil {
				log.Printf("CreateOffer error: %v", err)
				return
			}

			err = p.peer.SetLocalDescription(offer)
			if err != nil {
				log.Printf("SetLocalDescription error: %v", err)
				return
			}

			if err := conn.Notify(ctx, "offer", offer); err != nil {
				log.Printf("error sending offer %s", err)
			}
		})

		p.peer = peer

		_ = conn.Reply(ctx, req.ID, answer)

		// Hack until renegotation is supported in pion. Force renegotation incase there are unmatched
		// receviers (i.e. sfu has more than one sender). We just naively create server offer. It is
		// noop if things are already matched. We can remove once https://github.com/pion/webrtc/pull/1322
		// is merged
		time.Sleep(1000 * time.Millisecond)

		log.Printf("on negotiation needed called")
		offer, err := p.peer.CreateOffer()
		if err != nil {
			log.Printf("CreateOffer error: %v", err)
			return
		}

		err = p.peer.SetLocalDescription(offer)
		if err != nil {
			log.Printf("SetLocalDescription error: %v", err)
			return
		}

		if err := conn.Notify(ctx, "offer", offer); err != nil {
			log.Printf("error sending offer %s", err)
		}

	case "offer":
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s offer", p.peer.ID())

		var negotiation Negotiation
		err := json.Unmarshal(*req.Params, &negotiation)
		if err != nil {
			log.Printf("connect: error parsing offer: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		// Peer exists, renegotiating existing peer
		err = p.peer.SetRemoteDescription(negotiation.Desc)
		if err != nil {
			log.Printf("Offer error: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		answer, err := p.peer.CreateAnswer()
		if err != nil {
			log.Printf("Offer error: answer=%v err=%v", answer, err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		err = p.peer.SetLocalDescription(answer)
		if err != nil {
			log.Printf("Offer error: answer=%v err=%v", answer, err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		_ = conn.Reply(ctx, req.ID, answer)

	case "answer":
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s answer", p.peer.ID())

		var negotiation Negotiation
		err := json.Unmarshal(*req.Params, &negotiation)
		if err != nil {
			log.Printf("connect: error parsing answer: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		err = p.peer.SetRemoteDescription(negotiation.Desc)
		if err != nil {
			log.Printf("error setting remote description %s", err)
		}

	case "trickle":
		log.Printf("trickle")
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s trickle", p.peer.ID())

		var trickle Trickle
		err := json.Unmarshal(*req.Params, &trickle)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		err = p.peer.AddICECandidate(trickle.Candidate)
		if err != nil {
			log.Printf("error setting ice candidate %s", err)
		}

	case "stream":
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s get stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		_ = conn.Reply(ctx, req.ID, userIdToStream[streamInfo.Sid])

	case "screen_stream":
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s get screen stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		_ = conn.Reply(ctx, req.ID, userIdToScreenStream[streamInfo.Sid])

	case "register_stream":
		log.Printf("register_stream")
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s register stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		log.Printf("registered UserId %s StreamId %s", streamInfo.Uid, streamInfo.StreamId)

		if _, ok := userIdToStream[streamInfo.Sid]; ok {
			userIdToStream[streamInfo.Sid][streamInfo.Uid] = streamInfo.StreamId
		} else {
			userIdToStream[streamInfo.Sid] = make(map[string]string)
			userIdToStream[streamInfo.Sid][streamInfo.Uid] = streamInfo.StreamId
		}

		conn.Notify(ctx, "stream", userIdToStream)

	case "register_screen_stream":
		log.Printf("register_screen_stream")
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s register screen stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		log.Printf("registered UserId %s screenStreamId %s", streamInfo.Uid, streamInfo.StreamId)

		if _, ok := userIdToScreenStream[streamInfo.Sid]; ok {
			userIdToScreenStream[streamInfo.Sid][streamInfo.Uid] = streamInfo.StreamId
		} else {
			userIdToScreenStream[streamInfo.Sid] = make(map[string]string)
			userIdToScreenStream[streamInfo.Sid][streamInfo.Uid] = streamInfo.StreamId
		}

		conn.Notify(ctx, "screen_stream", userIdToScreenStream)

	case "remove_stream":
		log.Printf("remove_stream")
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s remove stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		log.Printf("remove UserId %s StreamId %s", streamInfo.Uid, streamInfo.StreamId)

		if _, ok := userIdToStream[streamInfo.Sid]; ok {
			delete( userIdToStream[streamInfo.Sid], streamInfo.Uid )
		}

		conn.Notify(ctx, "stream", userIdToStream)

	case "remove_screen_stream":
		log.Printf("remove_stream")
		if p.peer == nil {
			log.Printf("connect: no peer exists for connection")
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", errors.New("no peer exists")),
			})
			break
		}

		log.Printf("peer %s remove screen stream", p.peer.ID())

		var streamInfo StreamInfo

		err := json.Unmarshal(*req.Params, &streamInfo)
		if err != nil {
			log.Printf("connect: error parsing candidate: %v", err)
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    500,
				Message: fmt.Sprintf("%s", err),
			})
			break
		}

		log.Printf("remove UserId %s screenStreamId %s", streamInfo.Uid, streamInfo.StreamId)

		if _, ok := userIdToScreenStream[streamInfo.Sid]; ok {
			delete( userIdToScreenStream[streamInfo.Sid], streamInfo.Uid )
		}

		conn.Notify(ctx, "screen_stream", userIdToScreenStream)
	}
}

func main() {
	if !parse() {
		showHelp()
		os.Exit(-1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "7000"
	}

	log.Printf("Listening on port %s", port)

	//log.Infof("--- Starting SFU Node ---")
	rpc := NewRPC()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	http.Handle("/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}
		defer c.Close()

		p := &peerContext{}
		ctx := context.WithValue(r.Context(), peerCtxKey, p)
		jc := jsonrpc2.NewConn(ctx, websocketjsonrpc2.NewObjectStream(c), rpc)

		<-jc.DisconnectNotify()

		if p.peer != nil {
			log.Printf("Closing peer")
			p.peer.Close()
		}
	}))

	addr = ":"+port

	var err error
	if key != "" && cert != "" {
		log.Printf(" at https://[Listening%s]", addr)
		err = http.ListenAndServeTLS(addr, cert, key, nil)
	} else {
		log.Printf("Listening at http://[%s]", addr)
		err = http.ListenAndServe(addr, nil)
	}
	if err != nil {
		panic(err)
	}

	//appengine.Main()
}