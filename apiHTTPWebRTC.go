package main

import (
	"time"

	"github.com/deepch/vdk/av"
	webrtc "github.com/deepch/vdk/format/webrtcv3"
	"github.com/gin-gonic/gin"
	"github.com/gordonklaus/portaudio"
	"github.com/sirupsen/logrus"
	"gopkg.in/hraban/opus.v2"
)

// HTTPAPIServerStreamWebRTC stream video over WebRTC
func HTTPAPIServerStreamWebRTC(c *gin.Context) {
	requestLogger := log.WithFields(logrus.Fields{
		"module":  "http_webrtc",
		"stream":  c.Param("uuid"),
		"channel": c.Param("channel"),
		"func":    "HTTPAPIServerStreamWebRTC",
	})

	if !Storage.StreamChannelExist(c.Param("uuid"), c.Param("channel")) {
		c.IndentedJSON(500, Message{Status: 0, Payload: ErrorStreamNotFound.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "StreamChannelExist",
		}).Errorln(ErrorStreamNotFound.Error())
		return
	}

	if !RemoteAuthorization("WebRTC", c.Param("uuid"), c.Param("channel"), c.Query("token"), c.ClientIP()) {
		requestLogger.WithFields(logrus.Fields{
			"call": "RemoteAuthorization",
		}).Errorln(ErrorStreamUnauthorized.Error())
		return
	}

	Storage.StreamChannelRun(c.Param("uuid"), c.Param("channel"))
	codecs, err := Storage.StreamChannelCodecs(c.Param("uuid"), c.Param("channel"))
	if err != nil {
		c.IndentedJSON(500, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "StreamCodecs",
		}).Errorln(err.Error())
		return
	}
	muxerWebRTC := webrtc.NewMuxer(webrtc.Options{ICEServers: Storage.ServerICEServers(), ICEUsername: Storage.ServerICEUsername(), ICECredential: Storage.ServerICECredential(), PortMin: Storage.ServerWebRTCPortMin(), PortMax: Storage.ServerWebRTCPortMax()})
	answer, err := muxerWebRTC.WriteHeader(codecs, c.PostForm("data"))
	if err != nil {
		c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "WriteHeader",
		}).Errorln(err.Error())
		return
	}
	_, err = c.Writer.Write([]byte(answer))
	if err != nil {
		c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
		requestLogger.WithFields(logrus.Fields{
			"call": "Write",
		}).Errorln(err.Error())
		return
	}
	go func() {
		cid, ch, _, err := Storage.ClientAdd(c.Param("uuid"), c.Param("channel"), WEBRTC)
		if err != nil {
			c.IndentedJSON(400, Message{Status: 0, Payload: err.Error()})
			requestLogger.WithFields(logrus.Fields{
				"call": "ClientAdd",
			}).Errorln(err.Error())
			return
		}
		defer Storage.ClientDelete(c.Param("uuid"), cid, c.Param("channel"))
		var videoStart bool
		noVideo := time.NewTimer(10 * time.Second)
		for {
			select {
			case <-noVideo.C:
				// c.IndentedJSON(500, Message{Status: 0, Payload: ErrorStreamNoVideo.Error()})
				requestLogger.WithFields(logrus.Fields{
					"call": "ErrorStreamNoVideo",
				}).Errorln(ErrorStreamNoVideo.Error())
				return
			case pck := <-ch:
				if pck.IsKeyFrame {
					noVideo.Reset(10 * time.Second)
					videoStart = true
				}
				if !videoStart {
					continue
				}
				err = muxerWebRTC.WritePacket(*pck)
				if err != nil {
					requestLogger.WithFields(logrus.Fields{
						"call": "WritePacket",
					}).Errorln(err.Error())
					return
				}
			}
		}
	}()
}

// HTTPAPIServerStreamWebRTC stream audio over WebRTC
func HTTPAPIServerStreamWebRTCAudio(c *gin.Context) {
	const sampleRate = 48000 // audio sample rate
	const channels = 1       // mono; 2 for stereo
	const bufferSize = 1000
	pcm := make([]int16, 240)
	frameSize := len(pcm)
	frameSizeMs := float32(frameSize) / channels * 1000 / sampleRate
	log.Errorf("frame size: %d bytes (%f ms)", frameSize, frameSizeMs)

	if err := portaudio.Initialize(); err != nil {
		log.Fatalf("Failed to initialize PortAudio: %s", err)
	}
	defer portaudio.Terminate()

	stream, err := portaudio.OpenDefaultStream(1, 0, sampleRate, frameSize, pcm)
	if err != nil {
		log.Fatalf("Failed to open default stream: %s", err)
	}
	defer stream.Close()

	// Start the audio stream
	if err := stream.Start(); err != nil {
		log.Fatalf("Failed to start stream: %s", err)
	}
	defer stream.Stop()

	muxer := webrtc.NewMuxer(webrtc.Options{ICEServers: Storage.ServerICEServers(), ICEUsername: Storage.ServerICEUsername(), ICECredential: Storage.ServerICECredential(), PortMin: Storage.ServerWebRTCPortMin(), PortMax: Storage.ServerWebRTCPortMax()})

	log.Println("Audio track added to muxer")

	answer, err := muxer.WriteHeader(nil, c.PostForm("data"))
	_, err = c.Writer.Write([]byte(answer))

	for {
		// Create Opus encoder
		enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
		if err != nil {
			log.Fatalf("Failed to create Opus encoder: %s", err)
		}

		// Encode to Opus
		packet := make([]byte, bufferSize)
		n, err := enc.Encode(pcm, packet)
		if err != nil {
			log.Printf("Error encoding PCM to Opus: %s", err)
			return
		}
		avPacket := &av.Packet{
			IsKeyFrame: false,
			Idx:        1,
			Data:       packet[:n],
		}

		muxer.WritePacket(*avPacket)
		return
	}
}

func chk(err error) {
	if err != nil {
		panic(err)
	}
}
