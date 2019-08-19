package routing

import (
	"fmt"
	"time"

	"github.com/skycoin/dmsg/cipher"
)

// AddrLoop defines a loop over a pair of addresses.
type AddrLoop struct {
	Local  Addr
	Remote Addr
}

// TODO: discuss if we should add local PK to the output
func (l AddrLoop) String() string {
	return fmt.Sprintf("%s:%d <-> %s:%d", l.Local.PubKey, l.Local.Port, l.Remote.PubKey, l.Remote.Port)
}

// LoopDescriptor defines a loop over a pair of routes.
type LoopDescriptor struct {
	Loop    AddrLoop
	Forward Route
	Reverse Route
	Expiry  time.Time
}

// Initiator returns initiator of the Loop.
func (l LoopDescriptor) Initiator() cipher.PubKey {
	if len(l.Forward) == 0 {
		panic("empty forward route")
	}

	return l.Forward[0].From
}

// Responder returns responder of the Loop.
func (l LoopDescriptor) Responder() cipher.PubKey {
	if len(l.Reverse) == 0 {
		panic("empty reverse route")
	}

	return l.Reverse[0].From
}

func (l LoopDescriptor) String() string {
	return fmt.Sprintf("lport: %d. rport: %d. routes: %s/%s. expire at %s",
		l.Loop.Local.Port, l.Loop.Remote.Port, l.Forward, l.Reverse, l.Expiry)
}

// LoopData stores loop confirmation request data.
type LoopData struct {
	Loop    AddrLoop `json:"loop"`
	RouteID RouteID  `json:"resp-rid,omitempty"`
}
