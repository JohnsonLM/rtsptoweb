package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hajimehoshi/oto"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"gopkg.in/hraban/opus.v2"
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
	log.Println("Received SDP Offer")

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

	// Handle incoming audio from client
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Println("Track acquired", track.Kind(), track.Codec())
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			go playOpusAudio(track)
		}
	})

	// Create a audio track
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		log.Fatal("Failed to create audio track:", err)
	} else {
		log.Info("Created audio track")
	}

	rtpSender, audioTrackErr := peerConnection.AddTrack(audioTrack)
	if audioTrackErr != nil {
		log.Panic(audioTrackErr)
	} else {
		log.Info("Added audio track to conn")
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
		if state == webrtc.ICEGathererStateComplete {
			log.Println("ICE gathering complete.")
			wg.Done() // Signal that ICE gathering is complete
		} else {
			log.Println("ICE Gathering State: ", state.String())
		}
	})

	// Wait for ICE candidates to gather before responding
	wg.Wait()
	// Log the final SDP
	log.Print("Sending answer with gathered ICE candidates")

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
	log.Info("Starting tone generation...")

	// Tone parameters
	const sampleRate = 48000                    // Samples per second
	const frequency = 440.0                     // Tone frequency (Hz)
	const volume = 0.5                          // Amplitude (0.0 to 1.0)
	const frameDuration = time.Millisecond * 20 // Frame duration (20ms)

	// Calculate the number of samples per frame
	samplesPerFrame := int(sampleRate * frameDuration.Seconds())
	sampleBuffer := make([]byte, samplesPerFrame*2) // 16-bit PCM, 2 bytes per sample

	// Generate a sine wave for the frame
	for i := 0; i < samplesPerFrame; i++ {
		sample := int16(volume * math.MaxInt16 * math.Sin(2*math.Pi*frequency*float64(i)/float64(sampleRate)))
		sampleBuffer[i*2] = byte(sample & 0xff)          // Little-endian LSB
		sampleBuffer[i*2+1] = byte((sample >> 8) & 0xff) // Little-endian MSB
	}

	// Continuously write frames to the track
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	for range ticker.C {
		err := track.WriteSample(media.Sample{
			Data:     sampleBuffer,
			Duration: frameDuration,
		})
		if err != nil {
			log.Errorf("Error writing sample: %v", err)
			return
		}
	}
}

func playOpusAudio(track *webrtc.TrackRemote) {
	const (
		sampleRate     = 48000
		channelCount   = 2
		bytesPerSample = 2
		bufferSize     = 1024
	)

	context, err := oto.NewContext(sampleRate, channelCount, bytesPerSample, bufferSize)
	if err != nil {
		log.Println("Failed to initialize audio playback context:", err)
		return
	}
	defer context.Close()

	player := context.NewPlayer()
	defer player.Close()

	decoder, err := opus.NewDecoder(sampleRate, channelCount)
	if err != nil {
		log.Println("Failed to initialize Opus decoder:", err)
		return
	}

	audioBuffer := make([]byte, bufferSize)                     // Opus payload buffer
	pcmSamplesPerFrame := 960                                   // PCM samples per 20ms at 48 kHz for stereo audio
	pcmBuffer := make([]int16, pcmSamplesPerFrame*channelCount) // 960 * 2 (Stereo = 2 channels)

	for {
		// Read Opus packets
		n, _, readErr := track.Read(audioBuffer)
		if readErr != nil {
			log.Println("Error reading track data:", readErr)
			break
		}

		// Decode Opus payload into PCM data
		numSamples, decodeErr := decoder.Decode(audioBuffer[:n], pcmBuffer)
		if decodeErr != nil {
			log.Println("Decoder error:", decodeErr)
			continue
		}

		// Convert PCM to bytes
		pcmBytes := int16ToBytes(pcmBuffer[:numSamples*channelCount])

		// Write PCM to the player
		_, writeErr := player.Write(pcmBytes)
		if writeErr != nil {
			log.Println("Error writing audio to player:", writeErr)
			break
		}
	}
}

func int16ToBytes(samples []int16) []byte {
	bytes := make([]byte, len(samples)*2) // Each int16 sample has 2 bytes
	for i, sample := range samples {
		bytes[2*i] = byte(sample & 0xFF)          // LSB
		bytes[2*i+1] = byte((sample >> 8) & 0xFF) // MSB
	}
	return bytes
}
