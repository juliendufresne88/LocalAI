package openai

import (
	"net"
	"strings"
	"time"

	"github.com/mudler/LocalAI/core/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// This spec is a Linux (or any host) pion-to-pion check. Both peers are forced
// onto the loopback interface, so an up physical NIC is not on the candidate
// list. That is the stand-in for "no physical network" that this machine can
// run. It does not open a microphone, start a browser, or exercise macOS.
//
// ICE connectivity checks are STUN binding requests between the two agents.
// Matching the RTP payload after PeerConnectionStateConnected is the media
// check: pion only delivers that payload once DTLS has finished and SRTP has
// been decrypted. No STUN or TURN server is configured. pion v4.2.11 does not
// copy the DTLS state into TransportStats, so the spec uses the peer
// connection state, which pion updates from the DTLS transport.
var _ = Describe("loopback WebRTC media", func() {
	It("exchanges bidirectional Opus RTP over DTLS-SRTP on loopback only", func() {
		lo := loopbackInterfaceName()
		cfg := &config.ApplicationConfig{WebRTCICEInterfaces: []string{lo}}
		offerEngine, err := webRTCSettingEngine(cfg)
		Expect(err).NotTo(HaveOccurred())
		answerEngine, err := webRTCSettingEngine(cfg)
		Expect(err).NotTo(HaveOccurred())

		// The offerer stands in for the browser and the answerer for LocalAI.
		// A stock browser does not gather loopback; this peer does, on purpose,
		// so the server opt-in can be tested without Chrome.
		offerPC, offerTrack := newLoopbackAudioPeer(offerEngine, "browser-mic")
		answerPC, answerTrack := newLoopbackAudioPeer(answerEngine, "server-tts")
		DeferCleanup(offerPC.Close)
		DeferCleanup(answerPC.Close)

		micPayload := []byte("localai-loopback-mic-rtp")
		ttsPayload := []byte("localai-loopback-tts-rtp")
		gotMic := armRTPReader(answerPC)
		gotTTS := armRTPReader(offerPC)

		signalLoopback(offerPC, answerPC)

		Eventually(offerPC.ConnectionState, 15*time.Second, 50*time.Millisecond).
			Should(Equal(webrtc.PeerConnectionStateConnected))
		Eventually(answerPC.ConnectionState, 15*time.Second, 50*time.Millisecond).
			Should(Equal(webrtc.PeerConnectionStateConnected))
		// pion sets PeerConnectionStateConnected only after the DTLS transport
		// is connected or closed. A fresh session that just reached it, with
		// ICE connected or completed, is the connected case. ICETransport.Stats
		// in pion v4.2.11 does not fill TransportStats.DTLSState.
		Eventually(offerPC.ICEConnectionState, 5*time.Second, 50*time.Millisecond).
			Should(BeElementOf(webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted))
		Eventually(answerPC.ICEConnectionState, 5*time.Second, 50*time.Millisecond).
			Should(BeElementOf(webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted))

		assertLoopbackPair(offerPC)
		assertLoopbackPair(answerPC)

		writeProbe := func(track *webrtc.TrackLocalStaticRTP, payload []byte, seq uint16) {
			for i := 0; i < 5; i++ {
				Expect(track.WriteRTP(&rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    111,
						SequenceNumber: seq + uint16(i),
						Timestamp:      uint32(i) * 960,
						SSRC:           1,
					},
					Payload: payload,
				})).To(Succeed())
				time.Sleep(20 * time.Millisecond)
			}
		}
		writeProbe(offerTrack, micPayload, 1)
		writeProbe(answerTrack, ttsPayload, 100)

		Eventually(gotMic, 5*time.Second).Should(Receive(Equal(micPayload)))
		Eventually(gotTTS, 5*time.Second).Should(Receive(Equal(ttsPayload)))
	})

	// The offerer's host candidates are removed from the SDP the answerer sees.
	// Both agents still bind loopback, so the offerer can send a check to the
	// answerer's advertised 127.0.0.1. The answerer must learn that source as
	// a peer-reflexive candidate. This does not start a browser, and it does
	// not show that a browser with no local socket can connect.
	It("learns a peer-reflexive loopback address when the offer hides its candidates", func() {
		lo := loopbackInterfaceName()
		cfg := &config.ApplicationConfig{WebRTCICEInterfaces: []string{lo}}
		offerEngine, err := webRTCSettingEngine(cfg)
		Expect(err).NotTo(HaveOccurred())
		answerEngine, err := webRTCSettingEngine(cfg)
		Expect(err).NotTo(HaveOccurred())

		offerPC, _ := newLoopbackAudioPeer(offerEngine, "hidden-offer")
		answerPC, _ := newLoopbackAudioPeer(answerEngine, "loopback-answer")
		DeferCleanup(offerPC.Close)
		DeferCleanup(answerPC.Close)

		offer, err := offerPC.CreateOffer(nil)
		Expect(err).NotTo(HaveOccurred())
		gathered := webrtc.GatheringCompletePromise(offerPC)
		Expect(offerPC.SetLocalDescription(offer)).To(Succeed())
		Eventually(gathered, 10*time.Second).Should(BeClosed())

		fullOffer := offerPC.LocalDescription().SDP
		Expect(fullOffer).To(ContainSubstring("127.0.0.1"))
		hidden := stripHostCandidates(fullOffer)
		Expect(hidden).NotTo(ContainSubstring("a=candidate:"))

		Expect(answerPC.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  hidden,
		})).To(Succeed())
		answer, err := answerPC.CreateAnswer(nil)
		Expect(err).NotTo(HaveOccurred())
		gathered = webrtc.GatheringCompletePromise(answerPC)
		Expect(answerPC.SetLocalDescription(answer)).To(Succeed())
		Eventually(gathered, 10*time.Second).Should(BeClosed())
		Expect(answerPC.LocalDescription().SDP).To(ContainSubstring("127.0.0.1"))
		Expect(offerPC.SetRemoteDescription(*answerPC.LocalDescription())).To(Succeed())

		Eventually(answerPC.ConnectionState, 15*time.Second, 50*time.Millisecond).
			Should(Equal(webrtc.PeerConnectionStateConnected))
		Eventually(func() bool {
			return hasLoopbackPrflx(answerPC)
		}, 5*time.Second, 50*time.Millisecond).Should(BeTrue())
	})
})

func stripHostCandidates(sdp string) string {
	var b strings.Builder
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "a=candidate:") {
			continue
		}
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func hasLoopbackPrflx(pc *webrtc.PeerConnection) bool {
	for _, stat := range pc.GetStats() {
		cand, ok := stat.(webrtc.ICECandidateStats)
		if !ok || cand.CandidateType != webrtc.ICECandidateTypePrflx {
			continue
		}
		ip := net.ParseIP(cand.IP)
		if ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

func newLoopbackAudioPeer(engine webrtc.SettingEngine, streamID string) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP) {
	m := &webrtc.MediaEngine{}
	Expect(m.RegisterDefaultCodecs()).To(Succeed())
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(engine))
	// Empty configuration: no STUN server, no TURN server.
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	Expect(err).NotTo(HaveOccurred())

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", streamID)
	Expect(err).NotTo(HaveOccurred())
	sender, err := pc.AddTrack(track)
	Expect(err).NotTo(HaveOccurred())
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, readErr := sender.Read(buf); readErr != nil {
				return
			}
		}
	}()
	return pc, track
}

func armRTPReader(pc *webrtc.PeerConnection) <-chan []byte {
	ch := make(chan []byte, 8)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(pkt.Payload) == 0 {
				continue
			}
			select {
			case ch <- append([]byte(nil), pkt.Payload...):
			default:
			}
		}
	})
	return ch
}

func signalLoopback(offerPC, answerPC *webrtc.PeerConnection) {
	offer, err := offerPC.CreateOffer(nil)
	Expect(err).NotTo(HaveOccurred())
	gathered := webrtc.GatheringCompletePromise(offerPC)
	Expect(offerPC.SetLocalDescription(offer)).To(Succeed())
	Eventually(gathered, 10*time.Second).Should(BeClosed())

	Expect(answerPC.SetRemoteDescription(*offerPC.LocalDescription())).To(Succeed())
	answer, err := answerPC.CreateAnswer(nil)
	Expect(err).NotTo(HaveOccurred())
	gathered = webrtc.GatheringCompletePromise(answerPC)
	Expect(answerPC.SetLocalDescription(answer)).To(Succeed())
	Eventually(gathered, 10*time.Second).Should(BeClosed())
	Expect(offerPC.SetRemoteDescription(*answerPC.LocalDescription())).To(Succeed())
}

// assertLoopbackPair requires a nominated host pair on loopback. A
// server-reflexive or relay candidate would mean a STUN or TURN server
// was involved. The physical NIC must not appear: both peers were filtered
// to the loopback interface.
func assertLoopbackPair(pc *webrtc.PeerConnection) {
	cands := map[string]webrtc.ICECandidateStats{}
	var pair *webrtc.ICECandidatePairStats
	for _, stat := range pc.GetStats() {
		switch s := stat.(type) {
		case webrtc.ICECandidateStats:
			Expect(s.CandidateType).To(Equal(webrtc.ICECandidateTypeHost), "candidate %s", s.IP)
			parsed := net.ParseIP(s.IP)
			Expect(parsed).NotTo(BeNil())
			Expect(parsed.IsLoopback()).To(BeTrue(), "non-loopback candidate %s", s.IP)
			cands[s.ID] = s
		case webrtc.ICECandidatePairStats:
			if s.State != webrtc.StatsICECandidatePairStateSucceeded {
				continue
			}
			if pair == nil || (s.Nominated && !pair.Nominated) {
				chosen := s
				pair = &chosen
			}
		}
	}
	Expect(pair).NotTo(BeNil(), "no succeeded ICE pair")
	Expect(pair.Nominated).To(BeTrue())
	for _, id := range []string{pair.LocalCandidateID, pair.RemoteCandidateID} {
		cand, ok := cands[id]
		Expect(ok).To(BeTrue(), "missing candidate %s", id)
		Expect(net.ParseIP(cand.IP).IsLoopback()).To(BeTrue(), cand.IP)
	}
}
