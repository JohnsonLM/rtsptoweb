package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

func handleWebRTCStream(c *gin.Context) {
	// get sdp offer from json request
	bytedata, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Fatalf("Failed to read request body: %v", err)
	}
	reqBodyString := string(bytedata)
	offer := webrtc.SessionDescription{}
	decode(reqBodyString, &offer)
	fmt.Println("Received SDP Offer:", offer)

	// Configure PeerConnection with Google's public STUN servers
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{
					"stun:stun.l.google.com:19302", // Primary Google STUN server
					"stun:stun1.l.google.com:19302",
					"stun:stun2.l.google.com:19302",
				},
			},
		},
	})
	if err != nil {
		log.Fatalf("Failed to create PeerConnection: %v", err)
	}

	// Create a audio track
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		log.Fatal("Failed to create audio track:", err)
	}

	rtpSender, audioTrackErr := peerConnection.AddTrack(audioTrack)
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// Start generating tone
	go generateTone(audioTrack)

	// Handle incoming audio from client
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Received track: Codec=%s\n", track.Codec().MimeType)
	})

	// Take offer from remote, PeerConnection is now able to contact the other PeerConnection
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	// Create an Answer to send back to our originating PeerConnection
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// SetRemoteDescription on original PeerConnection, this finishes our signaling
	// bother PeerConnections should be able to communicate with each other now
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Wait until all ICE candidates have been gathered
	var wg sync.WaitGroup
	wg.Add(1)

	peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		fmt.Println("ICE Gathering State: %s\n", state.String())
		if state == webrtc.ICEGathererStateComplete {
			fmt.Println("ICE gathering complete.")
			wg.Done() // Signal that ICE gathering is complete
		}
	})

	// Wait for ICE candidates to gather before responding
	wg.Wait()
	// Log the final SDP
	fmt.Print("Sending answer with gathered ICE candidates")

	// Respond to client with the SDP answer
	c.JSON(http.StatusOK, answer)

	// Handle further processing asynchronously
	go func() {
		// Processing logic here
		select {} // Keep the peer connection open
	}()

}

// Decode a base64 and unmarshal JSON into a SessionDescription
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	err = json.Unmarshal(b, obj)
	if err != nil {
		panic(err)
	}
}

func generateTone(track *webrtc.TrackLocalStaticSample) {
	const sampleRate = 48000 // Audio sample rate
	const frequency = 440    // Frequency of the tone in Hz
	const durationMs = 20    // Duration per RTP packet in milliseconds

	// Prepare a buffer for samples
	samples := make([]byte, sampleRate*durationMs/1000*2) // PCM buffer: stereo 16-bit (2 bytes per sample)

	// Generate a sine wave
	for i := range samples {
		sineValue := int16(10000 * math.Sin(2*math.Pi*frequency*float64(i)/float64(sampleRate)))
		samples[i*2] = byte(sineValue & 0xFF)          // Low byte
		samples[i*2+1] = byte((sineValue >> 8) & 0xFF) // High byte
	}

	// Stream samples in continuous packets
	ticker := time.NewTicker(time.Millisecond * durationMs)
	defer ticker.Stop()

	for range ticker.C {
		err := track.WriteSample(media.Sample{Data: samples, Duration: 20 * time.Millisecond})
		if err != nil {
			log.Printf("Failed to send sample: %v", err)
		}
	}
}
