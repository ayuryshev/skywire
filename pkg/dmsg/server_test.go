package dmsg

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/nettest"

	"github.com/skycoin/skycoin/src/util/logging"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/messaging-discovery/client"
	"github.com/skycoin/skywire/pkg/transport"
)

// Ensure Server starts and quits with no error.
func TestNewServer(t *testing.T) {
	sPK, sSK := cipher.GenerateKeyPair()
	dc := client.NewMock()

	l, err := net.Listen("tcp", "")
	require.NoError(t, err)

	s, err := NewServer(sPK, sSK, l, dc)
	require.NoError(t, err)

	go s.Serve() //nolint:errcheck

	time.Sleep(time.Second)

	assert.NoError(t, s.Close())
}

func TestServer_Serve(t *testing.T) {
	sPK, sSK := cipher.GenerateKeyPair()
	dc := client.NewMock()

	l, err := nettest.NewLocalListener("tcp")
	//l, err := net.Listen("tcp", ":8089")
	require.NoError(t, err)

	s, err := NewServer(sPK, sSK, l, dc)
	require.NoError(t, err)

	go s.Serve()

	// connect two clients, establish transport, check if there are
	// two ServerConn's and that both conn's `nextLink` is filled correctly
	t.Run("test transport establishment", func(t *testing.T) {
		aPK, aSK := cipher.GenerateKeyPair()
		bPK, bSK := cipher.GenerateKeyPair()

		a := NewClient(aPK, aSK, dc)
		a.SetLogger(logging.MustGetLogger("A"))
		err := a.InitiateServerConnections(context.Background(), 1)
		require.NoError(t, err)

		b := NewClient(bPK, bSK, dc)
		b.SetLogger(logging.MustGetLogger("B"))
		err = b.InitiateServerConnections(context.Background(), 1)
		require.NoError(t, err)

		aDone := make(chan error)
		var aTransport transport.Transport
		go func() {
			// avoid ambiguity between this and the outer one
			var err error

			aTransport, err = a.Accept(context.Background())

			aDone <- err
			close(aDone)
		}()

		bTransport, err := b.Dial(context.Background(), aPK)
		require.NoError(t, err)

		err = <-aDone
		require.NoError(t, err)

		// must be 2 ServerConn's
		require.Equal(t, 2, len(s.conns))

		// must have ServerConn for A
		aServerConn, ok := s.conns[aPK]
		require.Equal(t, true, ok)
		require.Equal(t, aPK, aServerConn.remoteClient)

		// must have ServerConn for B
		bServerConn, ok := s.conns[bPK]
		require.Equal(t, true, ok)
		require.Equal(t, bPK, bServerConn.remoteClient)

		// must have a ClientConn
		aClientConn, ok := a.conns[sPK]
		require.Equal(t, true, ok)
		require.Equal(t, sPK, aClientConn.remoteSrv)

		// must have a ClientConn
		bClientConn, ok := b.conns[sPK]
		require.Equal(t, true, ok)
		require.Equal(t, sPK, bClientConn.remoteSrv)

		// check whether nextConn's contents are as must be
		bNextConn := bServerConn.nextConns[bClientConn.nextInitID-2]
		require.NotNil(t, bNextConn)
		require.Equal(t, bNextConn.id, aServerConn.nextRespID-2)

		// check whether nextConn's contents are as must be
		aNextConn := aServerConn.nextConns[aServerConn.nextRespID-2]
		require.NotNil(t, aNextConn)
		require.Equal(t, aNextConn.id, bClientConn.nextInitID-2)

		log.Printf("%v\n", s.conns)

		err = aTransport.Close()
		require.NoError(t, err)

		err = bTransport.Close()
		require.NoError(t, err)

		err = a.Close()
		require.NoError(t, err)

		err = b.Close()
		require.NoError(t, err)

		time.Sleep(5 * time.Second)

		require.Equal(t, 0, len(s.conns))
		require.Equal(t, 0, len(a.conns))
		require.Equal(t, 0, len(b.conns))
	})

	t.Run("test transport establishment concurrently", func(t *testing.T) {
		initiatorsCount := 4
		remotesCount := 4

		initiators := make([]*Client, 0, initiatorsCount)
		remotes := make([]*Client, 0, remotesCount)

		for i := 0; i < initiatorsCount; i++ {
			pk, sk := cipher.GenerateKeyPair()

			c := NewClient(pk, sk, dc)
			c.SetLogger(logging.MustGetLogger(fmt.Sprintf("Initiator %d", i)))
			err := c.InitiateServerConnections(context.Background(), 1)
			require.NoError(t, err)

			initiators = append(initiators, c)
		}

		for i := 0; i < remotesCount; i++ {
			pk, sk := cipher.GenerateKeyPair()

			c := NewClient(pk, sk, dc)
			c.SetLogger(logging.MustGetLogger(fmt.Sprintf("Remote %d", i)))
			err := c.InitiateServerConnections(context.Background(), 1)
			require.NoError(t, err)

			remotes = append(remotes, c)
		}

		rand := rand.New(rand.NewSource(time.Now().UnixNano()))

		usedRemotes := make(map[int]struct{})
		pickedRemotes := make([]int, 0, initiatorsCount)
		for range initiators {
			remote := rand.Intn(remotesCount)
			usedRemotes[remote] = struct{}{}
			pickedRemotes = append(pickedRemotes, remote)
		}

		acceptErrs := make(chan error, remotesCount)
		var remotesWG sync.WaitGroup
		remotesTps := make(map[int]transport.Transport, len(usedRemotes))
		remotesWG.Add(len(usedRemotes))
		for i, r := range remotes {
			if _, ok := usedRemotes[i]; ok {
				go func() {
					var (
						transport transport.Transport
						err       error
					)

					transport, err = r.Accept(context.Background())
					if err != nil {
						acceptErrs <- err
					}

					remotesTps[i] = transport

					remotesWG.Done()
				}()
			}
		}

		dialErrs := make(chan error, initiatorsCount)
		var initiatorsWG sync.WaitGroup
		initiatorsTps := make([]transport.Transport, initiatorsCount)
		initiatorsWG.Add(initiatorsCount)
		for i := range initiators {
			go func() {
				var (
					transport transport.Transport
					err       error
				)

				transport, err = initiators[i].Dial(context.Background(), remotes[pickedRemotes[i]].pk)
				if err != nil {
					dialErrs <- err
				}

				initiatorsTps = append(initiatorsTps, transport)

				initiatorsWG.Done()
			}()
		}

		initiatorsWG.Wait()
		close(dialErrs)
		err = <-dialErrs
		require.NoError(t, err)

		remotesWG.Done()
		close(acceptErrs)
		err = <-acceptErrs
		require.NoError(t, err)

		require.Equal(t, len(usedRemotes)+initiatorsCount, len(s.conns))

		/*err = <-aDone
		require.NoError(t, err)

		// must be 2 ServerConn's
		require.Equal(t, 2, len(s.conns))

		// must have ServerConn for A
		aServerConn, ok := s.conns[aPK]
		require.Equal(t, true, ok)
		require.Equal(t, aPK, aServerConn.remoteClient)

		// must have ServerConn for B
		bServerConn, ok := s.conns[bPK]
		require.Equal(t, true, ok)
		require.Equal(t, bPK, bServerConn.remoteClient)

		// must have a ClientConn
		aClientConn, ok := a.conns[sPK]
		require.Equal(t, true, ok)
		require.Equal(t, sPK, aClientConn.remoteSrv)

		// must have a ClientConn
		bClientConn, ok := b.conns[sPK]
		require.Equal(t, true, ok)
		require.Equal(t, sPK, bClientConn.remoteSrv)

		// check whether nextConn's contents are as must be
		bNextConn := bServerConn.nextConns[bClientConn.nextInitID-2]
		require.NotNil(t, bNextConn)
		require.Equal(t, bNextConn.id, aServerConn.nextRespID-2)

		// check whether nextConn's contents are as must be
		aNextConn := aServerConn.nextConns[aServerConn.nextRespID-2]
		require.NotNil(t, aNextConn)
		require.Equal(t, aNextConn.id, bClientConn.nextInitID-2)

		log.Printf("%v\n", s.conns)

		err = aTransport.Close()
		require.NoError(t, err)

		err = bTransport.Close()
		require.NoError(t, err)

		err = a.Close()
		require.NoError(t, err)

		err = b.Close()
		require.NoError(t, err)

		time.Sleep(5 * time.Second)

		require.Equal(t, 0, len(s.conns))
		require.Equal(t, 0, len(a.conns))
		require.Equal(t, 0, len(b.conns))*/
	})
}

// Given two client instances (a & b) and a server instance (s),
// Client b should be able to dial a transport with client b
// Data should be sent and delivered successfully via the transport.
// TODO: fix this.
func TestNewClient(t *testing.T) {
	aPK, aSK := cipher.GenerateKeyPair()
	bPK, bSK := cipher.GenerateKeyPair()
	sPK, sSK := cipher.GenerateKeyPair()
	sAddr := ":8081"

	const tpCount = 10
	const msgCount = 100

	dc := client.NewMock()

	l, err := net.Listen("tcp", sAddr)
	require.NoError(t, err)

	log.Println(l.Addr().String())

	s, err := NewServer(sPK, sSK, l, dc)
	require.NoError(t, err)

	go s.Serve() //nolint:errcheck

	a := NewClient(aPK, aSK, dc)
	a.SetLogger(logging.MustGetLogger("A"))
	require.NoError(t, a.InitiateServerConnections(context.Background(), 1))

	b := NewClient(bPK, bSK, dc)
	b.SetLogger(logging.MustGetLogger("B"))
	require.NoError(t, b.InitiateServerConnections(context.Background(), 1))

	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < tpCount; i++ {
			aDone := make(chan struct{})
			var aTp transport.Transport
			go func() {
				var err error
				aTp, err = a.Accept(context.Background())
				catch(err)
				close(aDone)
			}()

			bTp, err := b.Dial(context.Background(), aPK)
			catch(err)

			<-aDone
			catch(err)

			for j := 0; j < msgCount; j++ {
				pay := []byte(fmt.Sprintf("This is message %d!", j))
				_, err := aTp.Write(pay)
				catch(err)
				_, err = bTp.Read(pay)
				catch(err)
			}

			// Close TPs
			catch(aTp.Close())
			catch(bTp.Close())
		}
	}()

	for i := 0; i < tpCount; i++ {
		bDone := make(chan struct{})
		var bErr error
		var bTp transport.Transport
		go func() {
			bTp, bErr = b.Accept(context.Background())
			close(bDone)
		}()

		aTp, err := a.Dial(context.Background(), bPK)
		require.NoError(t, err)

		<-bDone
		require.NoError(t, bErr)

		for j := 0; j < msgCount; j++ {
			pay := []byte(fmt.Sprintf("This is message %d!", j))

			n, err := aTp.Write(pay)
			require.Equal(t, len(pay), n)
			require.NoError(t, err)

			got := make([]byte, len(pay))
			n, err = bTp.Read(got)
			require.Equal(t, len(pay), n)
			require.NoError(t, err)
			require.Equal(t, pay, got)
		}

		// Close TPs
		require.NoError(t, aTp.Close())
		require.NoError(t, bTp.Close())
	}
	wg.Wait()

	// Close server.
	assert.NoError(t, s.Close())
}

func catch(err error) {
	if err != nil {
		panic(err)
	}
}

func TestNewConn(t *testing.T) {

}
