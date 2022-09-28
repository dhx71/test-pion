package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

type WebrtcSession struct {
	mediaEngine           *webrtc.MediaEngine
	api                   *webrtc.API
	peerConnection        *webrtc.PeerConnection
	ICEConnected          bool
	outputTrack           *webrtc.TrackLocalStaticRTP //TrackLocalStaticSample
	packetizer            rtp.Packetizer
	markerSetOnce         bool
	iceConnectedCtx       context.Context
	iceConnectedCtxCancel context.CancelFunc
}

func NewWebrtcSession(offer webrtc.SessionDescription, remoteAudioHandler func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)) (*WebrtcSession, error) {
	var err error
	result := WebrtcSession{
		mediaEngine: &webrtc.MediaEngine{},
	}
	// Setup the codec PCMU
	if err = result.mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to setup codec. %s", err)
	}

	// Create the API object with the MediaEngine
	result.api = webrtc.NewAPI(webrtc.WithMediaEngine(result.mediaEngine) /*, webrtc.WithInterceptorRegistry(result.interceptor)*/)

	// Prepare the configuration
	config := webrtc.Configuration{
		/*ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},*/
	}

	// Create a new RTCPeerConnection
	result.peerConnection, err = result.api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create new peer connection. %s", err)
	}

	// Allow us to receive 1 audio track
	/*if _, err = result.peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to add audio transceiver. %s", err)
	}*/

	result.outputTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "pion")
	//result.outputTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "pion")
	if err != nil {
		return nil, fmt.Errorf("failed to create new local audio track. %s", err)
	}
	result.packetizer = rtp.NewPacketizer(
		1500,
		0,
		0,
		&codecs.G711Payloader{},
		rtp.NewRandomSequencer(),
		8000,
	)
	// Set a handler for when a new remote track starts
	result.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				if result.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed ||
					result.peerConnection.ConnectionState() == webrtc.PeerConnectionStateDisconnected ||
					result.peerConnection.ConnectionState() == webrtc.PeerConnectionStateFailed {
					log.Println("peer disconnected. ending RTCP loop")
					return
				}
				errSend := result.peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if errSend != nil {
					log.Printf("failed to send RTCP packet (conn state: %s). %s\n", result.peerConnection.ConnectionState(), errSend)
				}
			}
		}()

		codec := track.Codec()
		log.Printf("on track. codec: %++v\n", codec)
		// there is only one PCMU track
		go remoteAudioHandler(track, receiver)
	})
	rtpSender, err := result.peerConnection.AddTrack(result.outputTrack)
	if err != nil {
		return nil, fmt.Errorf("failed to add output track. %s", err)
	}
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	result.iceConnectedCtx, result.iceConnectedCtxCancel = context.WithCancel(context.Background())

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	result.peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			result.ICEConnected = true
			result.iceConnectedCtxCancel()
		} else if connectionState == webrtc.ICEConnectionStateFailed {
			// Gracefully shutdown the peer connection
			if closeErr := result.peerConnection.Close(); closeErr != nil {
				log.Printf("failed to close peer connection. %s\n", closeErr)
			}
			result.ICEConnected = false
		}
	})

	// Set the remote SessionDescription
	err = result.peerConnection.SetRemoteDescription(offer)
	if err != nil {
		return nil, fmt.Errorf("failed to set remote SDP. %s", err)
	}

	return &result, nil
}

func (webrtc *WebrtcSession) WaitICEConnected() {
	if webrtc.ICEConnected {
		return
	}
	<-webrtc.iceConnectedCtx.Done()
}

func (sess *WebrtcSession) Answer() (*webrtc.SessionDescription, error) {
	// Create answer
	answer, err := sess.peerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create answer. %s", err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(sess.peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = sess.peerConnection.SetLocalDescription(answer)
	if err != nil {
		return nil, fmt.Errorf("failed to set local SDP. %s", err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	return sess.peerConnection.LocalDescription(), nil
}

func (sess *WebrtcSession) Send(frame []byte) error {
	if !sess.ICEConnected {
		return fmt.Errorf("cannot send RTP yet, ICE not connected")
	}

	if sess.outputTrack == nil {
		return fmt.Errorf("no output track to send to")
	}

	/*pkt := make([]byte, 160)
	for i, linear := range frame {
		pkt[i] = byte(dsp.LinearToUlaw(int64(linear)))
	}*/
	packets := sess.packetizer.Packetize(frame, 160)
	for _, rtpPkt := range packets {
		if sess.markerSetOnce {
			rtpPkt.Marker = false
		} else {
			rtpPkt.Marker = true
			sess.markerSetOnce = true
		}

		err := sess.outputTrack.WriteRTP(rtpPkt)
		//err := sess.outputTrack.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond})
		if err != nil {
			return fmt.Errorf("failed to send rtp packet to browser. %s", err)
		}
	}
	return nil
}

func (sess *WebrtcSession) Close() error {
	return sess.peerConnection.Close()
}
