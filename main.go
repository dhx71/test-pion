package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-audio/wav"
	"github.com/pion/webrtc/v3"
)

//go:embed html/*
var content embed.FS
var theReceiver *webrtc.RTPReceiver

func main() {
	fmt.Println("==> http://localhost:8084/")
	subFS, _ := fs.Sub(content, "html")
	mutex := http.NewServeMux()
	mutex.Handle("/", http.FileServer(http.FS(subFS)))
	mutex.HandleFunc("/webrtc", handleWebrtc)
	err := http.ListenAndServe(":8084", mutex)
	if err != nil {
		log.Fatal(err)
	}
}

type WebRTCReq struct {
	BrowserSDP webrtc.SessionDescription
}

type WebRTCResp struct {
	ServerSDP *webrtc.SessionDescription
}

func handleWebrtc(w http.ResponseWriter, r *http.Request) {
	req := WebRTCReq{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sess, err := NewWebrtcSession(req.BrowserSDP, saveToFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := WebRTCResp{}
	resp.ServerSDP, err = sess.Answer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	go fromFileToBrowser(sess)
}

func saveToFile(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	theReceiver = receiver
	file, err := os.Create("output.wav")
	if err != nil {
		panic(err)
	}

	defer file.Close()
	enc := wav.NewEncoder(file, 8000, 8, 1, 7)
	defer enc.Close()

	fmt.Println("will save received audio into output.wav...")
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("failed to read rtp packet from browser, exiting loop. %s\n", err)
			return
		}
		if len(rtpPacket.Payload) == 0 {
			log.Println("stop reveiving RTP since read o bytes")
			break
		}
		fmt.Printf(".")
		for _, b := range rtpPacket.Payload {
			enc.WriteFrame(b)
		}

	}
}

func fromFileToBrowser(sess *WebrtcSession) {
	file, err := os.Open("input.wav")
	if err != nil {
		panic(err)
	}
	dec := wav.NewDecoder(file)
	err = dec.FwdToPCM()
	if err != nil {
		panic(err)
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		panic(err)
	}
	i := 0
	fmt.Printf("%v - %v\n\n\n\n", buf.Format, buf.Data)
	frame := make([]byte, 160)
	sendRTP := func() error {
		for j := 0; j < 160; j++ {
			frame[j] = byte(buf.Data[i+j])
		}
		i += 160
		return sess.Send(frame)
	}
	sess.WaitICEConnected()
	ticker := time.NewTicker(time.Millisecond * 20)
	for err == nil && i < len(buf.Data) {
		<-ticker.C
		err = sendRTP()
	}
	fmt.Printf("stop sending rtp packet to browser. %v\n", err)
	if theReceiver != nil {
		theReceiver.Stop()
	}
}
