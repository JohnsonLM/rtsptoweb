package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gordonklaus/portaudio"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"gopkg.in/hraban/opus.v2"
)

var (
	decoder *opus.Decoder
	err     error
)

const (
	sampleRate      = 48000
	inputChannels   = 2
	outputChannels  = 0
	framesPerBuffer = 960 // 20ms at 48kHz per channel
)

// function to handle WebRTC audio stream
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
			codec := track.Codec()
			if strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
				fmt.Println("Got Opus track, playing...")

				// Initialize PortAudio
				portaudio.Initialize()
				defer portaudio.Terminate()

				// Initialize the Opus decoder
				decoder, err = opus.NewDecoder(sampleRate, inputChannels) // 48000 Hz sample rate, 2 channel (stereo)
				if err != nil {
					log.Fatalf("failed to create Opus decoder: %v", err)
				}

				// Create a buffer to hold audio data
				buffer := make([]int16, framesPerBuffer*inputChannels)

				// Create a stream for audio playback
				stream, err := portaudio.OpenDefaultStream(outputChannels, inputChannels, sampleRate, len(buffer), &buffer)
				if err != nil {
					log.Fatal(err)
				}
				defer stream.Close()

				// Start the stream
				if err := stream.Start(); err != nil {
					log.Fatal(err)
				}
				defer stream.Stop()

				// WaitGroup to wait for the audio to finish playing
				var wg_audio sync.WaitGroup
				wg_audio.Add(1)

				defer wg_audio.Done()
				for {
					// Read RTP packets from the track
					packet, _, err := track.ReadRTP()
					if err != nil {
						log.Println(err)
						break
					}

					// Convert RTP packet to audio data and append to buffer
					audioData := convertRTPPacketToAudioData(packet)
					copy(buffer, audioData)
					//buffer = append(buffer, audioData...)

					// Write buffer to stream
					if err := stream.Write(); err != nil {
						log.Println(err)
						break
					}
				}

				// Wait for the audio to finish playing
				wg_audio.Wait()

				// Stop the stream
				if err := stream.Stop(); err != nil {
					log.Fatal(err)
				}
			}
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

// function to generate tone
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

// function to convert RTP packet to audio data
func convertRTPPacketToAudioData(packet *rtp.Packet) []int16 {
	// Decode the Opus packet to PCM
	pcm := make([]int16, framesPerBuffer*inputChannels)
	n, err := decoder.Decode(packet.Payload, pcm)
	if err != nil {
		log.Printf("failed to decode Opus packet: %v", err)
		return nil
	}
	return pcm[:n*inputChannels]
}
