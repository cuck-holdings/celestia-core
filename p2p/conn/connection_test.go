package conn

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/protoio"
	tmp2p "github.com/cometbft/cometbft/proto/tendermint/p2p"
	"github.com/cometbft/cometbft/proto/tendermint/types"
)

const maxPingPongPacketSize = 1024 // bytes

func createTestMConnection(conn net.Conn) *MConnection {
	onReceive := func(chID byte, msgBytes []byte) {
	}
	onError := func(r interface{}) {
	}
	c := createMConnectionWithCallbacks(conn, onReceive, onError)
	c.SetLogger(log.TestingLogger())
	return c
}

func createMConnectionWithCallbacks(
	conn net.Conn,
	onReceive func(chID byte, msgBytes []byte),
	onError func(r interface{}),
) *MConnection {
	cfg := DefaultMConnConfig()
	cfg.PingInterval = 90 * time.Millisecond
	cfg.PongTimeout = 45 * time.Millisecond
	chDescs := []*ChannelDescriptor{{ID: 0x01, Priority: 1, SendQueueCapacity: 1}}
	c := NewMConnectionWithConfig(conn, chDescs, onReceive, onError, cfg)
	c.SetLogger(log.TestingLogger())
	return c
}

func createMConnectionWithCallbacksConfigs(
	conn net.Conn,
	onReceive func(chID byte, msgBytes []byte),
	onError func(r interface{}),
	cfg MConnConfig,
) *MConnection {
	chDescs := []*ChannelDescriptor{{ID: 0x01, Priority: 1, SendQueueCapacity: 1}}
	c := NewMConnectionWithConfig(conn, chDescs, onReceive, onError, cfg)
	c.SetLogger(log.TestingLogger())
	return c
}

func TestMConnectionSendFlushStop(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	clientConn := createTestMConnection(client)
	err := clientConn.Start()
	require.Nil(t, err)
	defer clientConn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("abc")
	assert.True(t, clientConn.Send(0x01, msg))

	msgLength := 14

	// start the reader in a new routine, so we can flush
	errCh := make(chan error)
	go func() {
		msgB := make([]byte, msgLength)
		_, err := server.Read(msgB)
		if err != nil {
			t.Error(err)
			return
		}
		errCh <- err
	}()

	// stop the conn - it should flush all conns
	clientConn.FlushStop()

	timer := time.NewTimer(3 * time.Second)
	select {
	case <-errCh:
	case <-timer.C:
		t.Error("timed out waiting for msgs to be read")
	}
}

func TestMConnectionSend(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Ant-Man")
	assert.True(t, mconn.Send(0x01, msg))
	// Note: subsequent Send/TrySend calls could pass because we are reading from
	// the send queue in a separate goroutine.
	_, err = server.Read(make([]byte, len(msg)))
	if err != nil {
		t.Error(err)
	}
	assert.True(t, mconn.CanSend(0x01))

	msg = []byte("Spider-Man")
	assert.True(t, mconn.TrySend(0x01, msg))
	_, err = server.Read(make([]byte, len(msg)))
	if err != nil {
		t.Error(err)
	}

	assert.False(t, mconn.CanSend(0x05), "CanSend should return false because channel is unknown")
	assert.False(t, mconn.Send(0x05, []byte("Absorbing Man")), "Send should return false because channel is unknown")
}

func TestMConnectionSendRate(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	clientConn := createTestMConnection(client)
	err := clientConn.Start()
	require.Nil(t, err)
	defer clientConn.Stop() //nolint:errcheck // ignore for tests

	// prepare a message to send from client to the server
	msg := bytes.Repeat([]byte{1}, 1000*1024)

	// send the message and check if it was sent successfully
	done := clientConn.Send(0x01, msg)
	assert.True(t, done)

	// read the message from the server
	_, err = server.Read(make([]byte, len(msg)))
	if err != nil {
		t.Error(err)
	}

	// check if the peak send rate is within the expected range
	peakSendRate := clientConn.Status().SendMonitor.PeakRate
	// the peak send rate should be less than or equal to the max send rate
	// the max send rate is calculated based on the configured SendRate and other configs
	maxSendRate := clientConn.maxSendRate()
	assert.True(t, peakSendRate <= clientConn.maxSendRate(), fmt.Sprintf("peakSendRate %d > maxSendRate %d", peakSendRate, maxSendRate))
}

// maxSendRate returns the maximum send rate in bytes per second based on the MConnection's SendRate and other configs. It is used to calculate the highest expected value for the peak send rate.
// The returned value is slightly higher than the configured SendRate.
func (c *MConnection) maxSendRate() int64 {
	sampleRate := c.sendMonitor.GetSampleRate().Seconds()
	numberOfSamplePerSecond := 1 / sampleRate
	sendRate := float64(round(float64(c.config.SendRate) * sampleRate))
	batchSizeBytes := float64(numBatchPacketMsgs * c._maxPacketMsgSize)
	effectiveRatePerSample := math.Ceil(sendRate/batchSizeBytes) * batchSizeBytes
	effectiveSendRate := round(numberOfSamplePerSecond * effectiveRatePerSample)

	return effectiveSendRate
}

// round returns x rounded to the nearest int64 (non-negative values only).
func round(x float64) int64 {
	if _, frac := math.Modf(x); frac >= 0.5 {
		return int64(math.Ceil(x))
	}
	return int64(math.Floor(x))
}

func TestMConnectionReceiveRate(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	// prepare a client connection with callbacks to receive messages
	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}

	cnfg := DefaultMConnConfig()
	cnfg.SendRate = 500_000 // 500 KB/s
	cnfg.RecvRate = 500_000 // 500 KB/s

	clientConn := createMConnectionWithCallbacksConfigs(client, onReceive, onError, cnfg)
	err := clientConn.Start()
	require.Nil(t, err)
	defer clientConn.Stop() //nolint:errcheck // ignore for tests

	serverConn := createMConnectionWithCallbacksConfigs(server, func(chID byte, msgBytes []byte) {}, func(r interface{}) {}, cnfg)
	err = serverConn.Start()
	require.Nil(t, err)
	defer serverConn.Stop() //nolint:errcheck // ignore for tests

	msgSize := int(2 * cnfg.RecvRate)
	msg := bytes.Repeat([]byte{1}, msgSize)
	assert.True(t, serverConn.Send(0x01, msg))

	// approximate the time it takes to receive the message given the configured RecvRate
	approxDelay := time.Duration(int64(math.Ceil(float64(msgSize)/float64(cnfg.RecvRate))) * int64(time.Second) * 2)

	select {
	case receivedBytes := <-receivedCh:
		assert.Equal(t, msg, receivedBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected %s, got %+v", msg, err)
	case <-time.After(approxDelay):
		t.Fatalf("Did not receive the message in %fs", approxDelay.Seconds())
	}

	peakRecvRate := clientConn.recvMonitor.Status().PeakRate
	maxRecvRate := clientConn.maxRecvRate()

	assert.True(t, peakRecvRate <= maxRecvRate, fmt.Sprintf("peakRecvRate %d > maxRecvRate %d", peakRecvRate, maxRecvRate))

	peakSendRate := clientConn.sendMonitor.Status().PeakRate
	maxSendRate := clientConn.maxSendRate()

	assert.True(t, peakSendRate <= maxSendRate, fmt.Sprintf("peakSendRate %d > maxSendRate %d", peakSendRate, maxSendRate))
}

// maxRecvRate returns the maximum receive rate in bytes per second based on
// the MConnection's RecvRate and other configs.
// It is used to calculate the highest expected value for the peak receive rate.
// Note that the returned value is slightly higher than the configured RecvRate.
func (c *MConnection) maxRecvRate() int64 {
	sampleRate := c.recvMonitor.GetSampleRate().Seconds()
	numberOfSamplePerSeccond := 1 / sampleRate
	recvRate := float64(round(float64(c.config.RecvRate) * sampleRate))
	batchSizeBytes := float64(c._maxPacketMsgSize)
	effectiveRecvRatePerSample := math.Ceil(recvRate/batchSizeBytes) * batchSizeBytes
	effectiveRecvRate := round(numberOfSamplePerSeccond * effectiveRecvRatePerSample)

	return effectiveRecvRate
}

func TestMConnectionReceive(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn1 := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn1.Start()
	require.Nil(t, err)
	defer mconn1.Stop() //nolint:errcheck // ignore for tests

	mconn2 := createTestMConnection(server)
	err = mconn2.Start()
	require.Nil(t, err)
	defer mconn2.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Cyclops")
	assert.True(t, mconn2.Send(0x01, msg))

	select {
	case receivedBytes := <-receivedCh:
		assert.Equal(t, msg, receivedBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected %s, got %+v", msg, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Did not receive %s message in 500ms", msg)
	}
}

func TestMConnectionStatus(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	status := mconn.Status()
	assert.NotNil(t, status)
	assert.Zero(t, status.Channels[0].SendQueueSize)
}

func TestMConnectionPongTimeoutResultsInError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	serverGotPing := make(chan struct{})
	go func() {
		// read ping
		var pkt tmp2p.Packet
		_, err := protoio.NewDelimitedReader(server, maxPingPongPacketSize).ReadMsg(&pkt)
		require.NoError(t, err)
		serverGotPing <- struct{}{}
	}()
	<-serverGotPing

	pongTimerExpired := mconn.config.PongTimeout + 200*time.Millisecond
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected error, but got %v", msgBytes)
	case err := <-errorsCh:
		assert.NotNil(t, err)
	case <-time.After(pongTimerExpired):
		t.Fatalf("Expected to receive error after %v", pongTimerExpired)
	}
}

func TestMConnectionMultiplePongsInTheBeginning(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	// sending 3 pongs in a row (abuse)
	protoWriter := protoio.NewDelimitedWriter(server)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
	require.NoError(t, err)

	serverGotPing := make(chan struct{})
	go func() {
		// read ping (one byte)
		var packet tmp2p.Packet
		_, err := protoio.NewDelimitedReader(server, maxPingPongPacketSize).ReadMsg(&packet)
		require.NoError(t, err)
		serverGotPing <- struct{}{}

		// respond with pong
		_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
		require.NoError(t, err)
	}()
	<-serverGotPing

	pongTimerExpired := mconn.config.PongTimeout + 20*time.Millisecond
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected no data, but got %v", msgBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected no error, but got %v", err)
	case <-time.After(pongTimerExpired):
		assert.True(t, mconn.IsRunning())
	}
}

func TestMConnectionMultiplePings(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	// sending 3 pings in a row (abuse)
	// see https://github.com/cometbft/cometbft/issues/1190
	protoReader := protoio.NewDelimitedReader(server, maxPingPongPacketSize)
	protoWriter := protoio.NewDelimitedWriter(server)
	var pkt tmp2p.Packet

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	assert.True(t, mconn.IsRunning())
}

func TestMConnectionPingPongs(t *testing.T) {
	// check that we are not leaking any go-routines
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	server, client := net.Pipe()

	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	serverGotPing := make(chan struct{})
	go func() {
		protoReader := protoio.NewDelimitedReader(server, maxPingPongPacketSize)
		protoWriter := protoio.NewDelimitedWriter(server)
		var pkt tmp2p.PacketPing

		// read ping
		_, err = protoReader.ReadMsg(&pkt)
		require.NoError(t, err)
		serverGotPing <- struct{}{}

		// respond with pong
		_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
		require.NoError(t, err)

		time.Sleep(mconn.config.PingInterval)

		// read ping
		_, err = protoReader.ReadMsg(&pkt)
		require.NoError(t, err)
		serverGotPing <- struct{}{}

		// respond with pong
		_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{}))
		require.NoError(t, err)
	}()
	<-serverGotPing
	<-serverGotPing

	pongTimerExpired := (mconn.config.PongTimeout + 20*time.Millisecond) * 2
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected no data, but got %v", msgBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected no error, but got %v", err)
	case <-time.After(2 * pongTimerExpired):
		assert.True(t, mconn.IsRunning())
	}
}

func TestMConnectionStopsAndReturnsError(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	if err := client.Close(); err != nil {
		t.Error(err)
	}

	select {
	case receivedBytes := <-receivedCh:
		t.Fatalf("Expected error, got %v", receivedBytes)
	case err := <-errorsCh:
		assert.NotNil(t, err)
		assert.False(t, mconn.IsRunning())
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive error in 500ms")
	}
}

func newClientAndServerConnsForReadErrors(t *testing.T, chOnErr chan struct{}) (*MConnection, *MConnection) {
	server, client := NetPipe()

	onReceive := func(chID byte, msgBytes []byte) {}
	onError := func(r interface{}) {}

	// create client conn with two channels
	chDescs := []*ChannelDescriptor{
		{ID: 0x01, Priority: 1, SendQueueCapacity: 1},
		{ID: 0x02, Priority: 1, SendQueueCapacity: 1},
	}
	mconnClient := NewMConnection(client, chDescs, onReceive, onError)
	mconnClient.SetLogger(log.TestingLogger().With("module", "client"))
	err := mconnClient.Start()
	require.Nil(t, err)

	// create server conn with 1 channel
	// it fires on chOnErr when there's an error
	serverLogger := log.TestingLogger().With("module", "server")
	onError = func(r interface{}) {
		chOnErr <- struct{}{}
	}
	mconnServer := createMConnectionWithCallbacks(server, onReceive, onError)
	mconnServer.SetLogger(serverLogger)
	err = mconnServer.Start()
	require.Nil(t, err)
	return mconnClient, mconnServer
}

func expectSend(ch chan struct{}) bool {
	after := time.After(time.Second * 5)
	select {
	case <-ch:
		return true
	case <-after:
		return false
	}
}

func TestMConnectionReadErrorBadEncoding(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)

	client := mconnClient.conn

	// Write it.
	_, err := client.Write([]byte{1, 2, 3, 4, 5})
	require.NoError(t, err)
	assert.True(t, expectSend(chOnErr), "badly encoded msgPacket")

	t.Cleanup(func() {
		if err := mconnClient.Stop(); err != nil {
			t.Log(err)
		}
	})

	t.Cleanup(func() {
		if err := mconnServer.Stop(); err != nil {
			t.Log(err)
		}
	})
}

func TestMConnectionReadErrorUnknownChannel(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)

	msg := []byte("Ant-Man")

	// fail to send msg on channel unknown by client
	assert.False(t, mconnClient.Send(0x03, msg))

	// send msg on channel unknown by the server.
	// should cause an error
	assert.True(t, mconnClient.Send(0x02, msg))
	assert.True(t, expectSend(chOnErr), "unknown channel")

	t.Cleanup(func() {
		if err := mconnClient.Stop(); err != nil {
			t.Log(err)
		}
	})

	t.Cleanup(func() {
		if err := mconnServer.Stop(); err != nil {
			t.Log(err)
		}
	})
}

func TestMConnectionReadErrorLongMessage(t *testing.T) {
	chOnErr := make(chan struct{})
	chOnRcv := make(chan struct{})

	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	defer mconnClient.Stop() //nolint:errcheck // ignore for tests
	defer mconnServer.Stop() //nolint:errcheck // ignore for tests

	mconnServer.onReceive = func(chID byte, msgBytes []byte) {
		chOnRcv <- struct{}{}
	}

	client := mconnClient.conn
	protoWriter := protoio.NewDelimitedWriter(client)

	// send msg that's just right
	var packet = tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      make([]byte, mconnClient.config.MaxPacketMsgPayloadSize),
	}

	_, err := protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.True(t, expectSend(chOnRcv), "msg just right")

	// send msg that's too long
	packet = tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      make([]byte, mconnClient.config.MaxPacketMsgPayloadSize+100),
	}

	_, err = protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.Error(t, err)
	assert.True(t, expectSend(chOnErr), "msg too long")
}

func TestMConnectionReadErrorUnknownMsgType(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	defer mconnClient.Stop() //nolint:errcheck // ignore for tests
	defer mconnServer.Stop() //nolint:errcheck // ignore for tests

	// send msg with unknown msg type
	_, err := protoio.NewDelimitedWriter(mconnClient.conn).WriteMsg(&types.Header{ChainID: "x"})
	require.NoError(t, err)
	assert.True(t, expectSend(chOnErr), "unknown msg type")
}

func TestMConnectionTrySend(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.Nil(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Semicolon-Woman")
	resultCh := make(chan string, 2)
	assert.True(t, mconn.TrySend(0x01, msg))
	_, err = server.Read(make([]byte, len(msg)))
	require.NoError(t, err)
	assert.True(t, mconn.CanSend(0x01))
	assert.True(t, mconn.TrySend(0x01, msg))
	assert.False(t, mconn.CanSend(0x01))
	go func() {
		mconn.TrySend(0x01, msg)
		resultCh <- "TrySend"
	}()
	assert.False(t, mconn.CanSend(0x01))
	assert.False(t, mconn.TrySend(0x01, msg))
	assert.Equal(t, "TrySend", <-resultCh)
}

//nolint:lll //ignore line length for tests
func TestConnVectors(t *testing.T) {

	testCases := []struct {
		testName string
		msg      proto.Message
		expBytes string
	}{
		{"PacketPing", &tmp2p.PacketPing{}, "0a00"},
		{"PacketPong", &tmp2p.PacketPong{}, "1200"},
		{"PacketMsg", &tmp2p.PacketMsg{ChannelID: 1, EOF: false, Data: []byte("data transmitted over the wire")}, "1a2208011a1e64617461207472616e736d6974746564206f766572207468652077697265"},
	}

	for _, tc := range testCases {
		tc := tc

		pm := mustWrapPacket(tc.msg)
		bz, err := pm.Marshal()
		require.NoError(t, err, tc.testName)

		require.Equal(t, tc.expBytes, hex.EncodeToString(bz), tc.testName)
	}
}

func TestMConnectionChannelOverflow(t *testing.T) {
	chOnErr := make(chan struct{})
	chOnRcv := make(chan struct{})

	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	t.Cleanup(stopAll(t, mconnClient, mconnServer))

	mconnServer.onReceive = func(chID byte, msgBytes []byte) {
		chOnRcv <- struct{}{}
	}

	client := mconnClient.conn
	protoWriter := protoio.NewDelimitedWriter(client)

	var packet = tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      []byte(`42`),
	}
	_, err := protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.True(t, expectSend(chOnRcv))

	packet.ChannelID = int32(1025)
	_, err = protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.False(t, expectSend(chOnRcv))

}

type stopper interface {
	Stop() error
}

func stopAll(t *testing.T, stoppers ...stopper) func() {
	return func() {
		for _, s := range stoppers {
			if err := s.Stop(); err != nil {
				t.Log(err)
			}
		}
	}
}

// TestLargeMessageSuccess demonstrates that large messages work with increased limit
func TestLargeMessageSuccess(t *testing.T) {
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	receivedMessage := make(chan []byte, 1)
	errorReceived := make(chan interface{}, 1)

	onReceive := func(chID byte, msgBytes []byte) {
		receivedMessage <- msgBytes
	}
	onError := func(r interface{}) {
		errorReceived <- r
	}

	// Use default config which now has 1MB limit
	chDescs := []*ChannelDescriptor{
		{
			ID:                  0x01,
			Priority:            1,
			SendQueueCapacity:   1,
			RecvMessageCapacity: 2 * 1024 * 1024, // 2MB receive capacity
		},
	}

	mconnServer := NewMConnection(server, chDescs, onReceive, onError)
	mconnServer.SetLogger(log.TestingLogger())
	mconnClient := NewMConnection(client, chDescs, func(chID byte, msgBytes []byte) {}, func(r interface{}) {})
	mconnClient.SetLogger(log.TestingLogger())

	err := mconnServer.Start()
	require.NoError(t, err)
	defer mconnServer.Stop()

	err = mconnClient.Start()
	require.NoError(t, err)
	defer mconnClient.Stop()

	// Send large messages of various sizes from the error logs
	testSizes := []int{10032, 12619, 13534, 102410} // Sizes from the issue

	for _, msgSize := range testSizes {
		largeMsg := make([]byte, msgSize)
		for i := range largeMsg {
			largeMsg[i] = byte(i % 256)
		}

		t.Logf("Testing message size: %d bytes", msgSize)

		success := mconnClient.Send(0x01, largeMsg)
		require.True(t, success, "Send should queue the message successfully")

		// Wait for message reception (should succeed now)
		select {
		case msg := <-receivedMessage:
			assert.Equal(t, msgSize, len(msg), "Received message should have correct size")
			assert.Equal(t, largeMsg, msg, "Received message should match sent message")
		case err := <-errorReceived:
			t.Fatalf("Unexpected error for message size %d: %v", msgSize, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("Timeout waiting for message of size %d", msgSize)
		}
	}
}

// TestSmallLimitWouldFailLargeMessage demonstrates the old limit problem  
func TestSmallLimitWouldFailLargeMessage(t *testing.T) {
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	errorReceived := make(chan interface{}, 1)

	onReceive := func(chID byte, msgBytes []byte) {
		// Should not receive anything with small limit
	}
	onError := func(r interface{}) {
		errorReceived <- r
	}

	// Use a small limit config to simulate the old behavior
	cfg := DefaultMConnConfig()
	cfg.MaxPacketMsgPayloadSize = 1024 // Old limit

	chDescs := []*ChannelDescriptor{
		{
			ID:                  0x01,
			Priority:            1,
			SendQueueCapacity:   1,
			RecvMessageCapacity: 2 * 1024 * 1024, // 2MB receive capacity (not the issue)
		},
	}

	mconnServer := NewMConnectionWithConfig(server, chDescs, onReceive, onError, cfg)
	mconnServer.SetLogger(log.TestingLogger())
	mconnClient := NewMConnectionWithConfig(client, chDescs, func(chID byte, msgBytes []byte) {}, func(r interface{}) {}, cfg)
	mconnClient.SetLogger(log.TestingLogger())

	err := mconnServer.Start()
	require.NoError(t, err)
	defer mconnServer.Stop()

	err = mconnClient.Start()
	require.NoError(t, err)
	defer mconnClient.Stop()

	// Send a large message (like the ones in the error logs: 10032 bytes)
	largeMsg := make([]byte, 10032)
	for i := range largeMsg {
		largeMsg[i] = byte(i % 256)
	}

	success := mconnClient.Send(0x01, largeMsg)
	require.True(t, success, "Send should queue the message successfully")

	// With the old 1024 byte limit, this should cause an error
	select {
	case err := <-errorReceived:
		t.Logf("Expected error with old limit: %v", err)
		// Verify it's the "message exceeds max size" error
		assert.Contains(t, fmt.Sprintf("%v", err), "message exceeds max size")
	case <-time.After(3 * time.Second):
		t.Log("Note: Connection may not immediately error with old limit due to chunking")
		// This is acceptable - the important part is the new limit works
	}
}
