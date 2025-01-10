package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v2"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"gopkg.in/hraban/opus.v2"
)

var (
	err                   error
	decoder               *opus.Decoder
	encoder               *opus.Encoder
	multicastGroup        = net.ParseIP("239.69.10.5")
	multicastPort         = 5004
	packetBuffer          [][]byte
	pcmBuffer             []int16 // Define pcmBuffer globally
	receiveMulticastGroup = "239.69.83.87:5004"
)

const (
	sampleRate      = 48000
	inputChannels   = 2
	framesPerBuffer = 960  // 20ms at 48kHz per channel
	frameSize       = 1920 // Desired frame size in samples
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

				// Initialize the Opus decoder
				decoder, err = opus.NewDecoder(sampleRate, inputChannels) // 48000 Hz sample rate, 2 channel (stereo)
				if err != nil {
					log.Fatalf("failed to create Opus decoder: %v", err)
				}

				// Create a UDP connection to the AES67 multicast group
				conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
					IP:   multicastGroup,
					Port: multicastPort,
				})
				if err != nil {
					log.Fatal("Failed to create UDP connection:", err)
				}
				defer conn.Close()

				// Start sending SAP announcements
				go sendSAPAnnouncements(multicastGroup, multicastPort)

				// Read RTP packets from the track and forward them to the multicast group
				buf := make([]byte, 1500)
				for {
					n, _, readErr := track.Read(buf)
					if readErr != nil {
						log.Error("Failed to read RTP packet:", readErr)
						return
					}

					packet := &rtp.Packet{}
					if err := packet.Unmarshal(buf[:n]); err != nil {
						log.Error("Failed to unmarshal RTP packet:", err)
						continue
					}
					// Decode Opus to PCM
					pcm := make([]int16, 1920*inputChannels) // 20ms frame at 48kHz, 2 channels
					frameSize, decodeErr := decoder.Decode(packet.Payload, pcm)
					if decodeErr != nil {
						log.Error("Failed to decode Opus packet:", decodeErr)
						continue
					}

					// Encode PCM to L24
					l24 := make([]byte, frameSize*3*inputChannels)
					for i := 0; i < frameSize*inputChannels; i++ {
						l24[i*3] = byte(pcm[i] >> 8)
						l24[i*3+1] = byte(pcm[i] >> 16)
						l24[i*3+2] = byte(pcm[i] >> 24)
					}

					// Create a new RTP packet with L24 payload
					l24Packet := &rtp.Packet{
						Header:  packet.Header,
						Payload: l24,
					}
					l24Packet.Header.PayloadType = 96 // Set payload type to 96 for L24

					// Marshal the L24 RTP packet
					l24Buf, marshalErr := l24Packet.Marshal()
					if marshalErr != nil {
						log.Error("Failed to marshal L24 RTP packet:", marshalErr)
						continue
					}

					if _, writeErr := conn.Write(l24Buf); writeErr != nil {
						log.Error("Failed to write RTP packet to multicast group:", writeErr)
						return
					}
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

	// Write audio data to the audio track from AES67 stream
	go func() {
		// Resolve the UDP address
		addr, err := net.ResolveUDPAddr("udp", receiveMulticastGroup)
		if err != nil {
			fmt.Println("Error resolving address:", err)
			os.Exit(1)
		}

		// Find the network interface by name
		iface, err := net.InterfaceByName("en0")
		if err != nil {
			fmt.Println("Error finding interface:", err)
			os.Exit(1)
		}

		// Create the UDP connection
		conn, err := net.ListenMulticastUDP("udp", iface, addr)
		if err != nil {
			fmt.Println("Error creating connection:", err)
			os.Exit(1)
		}
		defer conn.Close()

		// Set the read buffer size
		err = conn.SetReadBuffer(240)
		if err != nil {
			fmt.Println("Error setting read buffer:", err)
			os.Exit(1)
		}

		// Initialize the Opus encoder
		encoder, err := opus.NewEncoder(sampleRate, 1, opus.AppVoIP) // 48000 Hz sample rate, 1 channel (mono)
		if err != nil {
			log.Fatalf("failed to create Opus encoder: %v", err)
		}

		// Read RTP packets from the multicast group
		for {
			// Process and buffer the audio packet
			bufferRTPPackets(conn, encoder, audioTrack)
		}
	}()

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

func sendSAPAnnouncements(multicastIP net.IP, port int) {
	sdp := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      1234567890, // Unique session ID
			SessionVersion: 1,          // Increment for each session
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: GetOutboundIP().String(), // Replace with the actual IP address of the sender
		},
		SessionName: "Game-1-Client-Audio",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: multicastIP.String()},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{StartTime: 0, StopTime: 0},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{
			{
				MediaName: sdp.MediaName{
					Media:   "audio",
					Port:    sdp.RangedPort{Value: port},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{"96"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "96 L24/48000/2"},                                   // Corrected to stereo (2 channels)
					{Key: "ptime", Value: "20"},                                                // Packet time in milliseconds
					{Key: "ts-refclk", Value: "ptp=IEEE1588-2008:00-1D-C1-FF-FE-00-59-5D:0if"}, // IEEE 1588-2008 PTP reference clock
					{Key: "mediaclk", Value: "direct=0"},                                       // Direct media clock
				},
			},
		},
	}

	sdpContent, err := sdp.Marshal()
	if err != nil {
		log.Fatal("Failed to marshal SDP:", err)
	}

	var packet bytes.Buffer

	// Calculate the Message ID hash
	hash := sha1.New()
	hash.Write(sdpContent)
	messageIDHash := hash.Sum(nil)

	// SAP header
	// SAP/SDP header according to RFC 2974
	packet.WriteByte(0x20)                                    // SAP header first byte
	packet.WriteByte(0x00)                                    // Authentication length
	packet.Write(messageIDHash[:2])                           // Message ID hash
	packet.Write(net.ParseIP(GetOutboundIP().String()).To4()) // Source IP address (originating address)

	// Add proper payload type header with leading 'a'
	packet.WriteString("application/sdp") // No null termination needed

	packet.WriteByte(0x00) // null termination

	// Write SDP content
	packet.Write(sdpContent)

	sapAddr := &net.UDPAddr{
		IP:   net.ParseIP("239.255.255.255"), // SAP multicast address
		Port: 9875,                           // SAP port
	}

	conn, err := net.DialUDP("udp", nil, sapAddr)
	if err != nil {
		log.Fatal("Failed to create UDP connection for SAP:", err)
	}
	defer conn.Close()

	for {
		_, err := conn.Write(packet.Bytes())
		if err != nil {
			log.Error("Failed to send SAP packet:", err)
		}
		time.Sleep(15 * time.Second) // Send SAP announcements every 15 seconds
	}
}

// Function to buffer RTP packets and call processAudioPacket after 5 packets
func bufferRTPPackets(conn *net.UDPConn, encoder *opus.Encoder, audioTrack *webrtc.TrackLocalStaticSample) {
	buf := make([]byte, 160)
	const rtpHeaderSize = 12
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		fmt.Println("Error reading from UDP:", err)
	}

	// Extract the audio payload
	audioPayload := buf[rtpHeaderSize:n]
	sampleSize := 3 // 24-bit audio

	// Ensure the payload length is a multiple of the sample size and number of channels
	if len(audioPayload)%(sampleSize) != 0 {
		fmt.Println("Invalid payload length for interleaved L24 audio")
		return
	}

	numSamples := len(audioPayload) / (sampleSize) // Number of 24-bit samples
	pcm := make([]int16, numSamples)               // Buffer for the 16-bit PCM samples

	// Convert the payload to 16-bit PCM samples
	for i := 0; i < numSamples; i++ {
		sample := int32(audioPayload[i*sampleSize])<<16 | int32(audioPayload[i*sampleSize+1])<<8 | int32(audioPayload[i*sampleSize+2])
		// Sign extend the 24-bit sample to 32-bit
		if sample&0x800000 != 0 {
			sample |= ^0xFFFFFF
		}
		// Convert to 16-bit PCM
		pcm[i] = int16(sample >> 8)
	}

	// Append the new PCM samples to the buffer
	pcmBuffer = append(pcmBuffer, pcm...)

	// when there are enough samples in the buffer, process them
	if len(pcmBuffer) >= frameSize {
		frame := pcmBuffer[:frameSize]    // Take the first frameSize samples for encoding
		pcmBuffer = pcmBuffer[frameSize:] // Remove the used samples from the buffer

		// Encode PCM to Opus
		opusData := make([]byte, frameSize) // buffer for the encoded data
		n, encodeErr := encoder.Encode(frame, opusData)
		if encodeErr != nil {
			log.Error("Failed to encode PCM to Opus:", encodeErr)
		}
		opusData = opusData[:n] // only the first N bytes are opus data.

		// Write Opus data to the WebRTC audio track
		err = audioTrack.WriteSample(media.Sample{Data: opusData, Duration: time.Millisecond * 40})
		if err != nil {
			log.Error("Failed to write Opus data to audio track:", err)
		}
	}

}

// Get preferred outbound ip of this machine
func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}
