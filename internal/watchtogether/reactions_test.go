package watchtogether

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/cache"
)

func reactionsIn(conn *recordingConn) []Reaction {
	var reactions []Reaction
	for _, payload := range conn.payloads {
		if reaction, ok := payload[reactionMessageType].(Reaction); ok && payload[clusterMessageTypeKey] == reactionMessageType {
			reactions = append(reactions, reaction)
		}
	}
	return reactions
}

func TestReactionsReachEveryMemberAndAreRateLimited(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.member("guest").displayName = "Asha"

	if err := f.s.SendReactionForConnection(f.reg("guest"), ReactionBRB); err != nil {
		t.Fatalf("SendReactionForConnection = %v", err)
	}
	for _, profile := range []string{"host", "guest"} {
		got := reactionsIn(f.conns[profile])
		if len(got) != 1 || got[0].Preset != ReactionBRB || got[0].From != (MemberIdentity{UserID: 8, ProfileID: "guest", DisplayName: "Asha"}) {
			t.Fatalf("%s received %+v; want one brb from Asha", profile, got)
		}
	}

	if err := f.s.SendReactionForConnection(f.reg("guest"), "free text"); !errors.Is(err, ErrInvalidReaction) {
		t.Fatalf("free-text reaction error = %v; want ErrInvalidReaction", err)
	}
	// A socket the member no longer holds cannot speak for it.
	stale := registrationFor(f.repo.room.ID, 8, "guest", new(recordingConn))
	if err := f.s.SendReactionForConnection(stale, ReactionBack); !errors.Is(err, ErrRoomForbidden) {
		t.Fatalf("stale socket reaction error = %v; want ErrRoomForbidden", err)
	}

	// The burst is three: two more go through, the fourth is refused, and one
	// more is allowed a refill interval later. The budget is the member's own.
	for i := range reactionBurst - 1 {
		if err := f.s.SendReactionForConnection(f.reg("guest"), ReactionLaugh); err != nil {
			t.Fatalf("reaction %d within the burst = %v", i+2, err)
		}
	}
	if err := f.s.SendReactionForConnection(f.reg("guest"), ReactionLaugh); !errors.Is(err, ErrReactionRateLimited) {
		t.Fatalf("reaction past the burst error = %v; want ErrReactionRateLimited", err)
	}
	if err := f.s.SendReactionForConnection(f.reg("host"), ReactionLaugh); err != nil {
		t.Fatalf("host reaction while the guest is limited = %v", err)
	}
	f.now = f.now.Add(reactionInterval)
	if err := f.s.SendReactionForConnection(f.reg("guest"), ReactionBack); err != nil {
		t.Fatalf("reaction after a refill interval = %v", err)
	}
	if got := len(reactionsIn(f.conns["host"])); got != reactionBurst+2 {
		t.Fatalf("host received %d reactions; want %d", got, reactionBurst+2)
	}
}

func TestPauseWithReactionNamesThePause(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.repo.room.GuestControlPolicy = GuestControlPolicyGuestPlayPause
	f.s.rooms[f.repo.room.ID].room.GuestControlPolicy = GuestControlPolicyGuestPlayPause
	f.member("guest").displayName = "Asha"

	if _, err := f.s.HandleTransportRequestForConnection(context.Background(), f.reg("guest"), 8, "guest", TransportRequest{
		Action: TransportActionSeek, PositionSeconds: new(50.0), Reaction: ReactionBRB,
	}); !errors.Is(err, ErrInvalidReaction) {
		t.Fatalf("seek with a reaction error = %v; want ErrInvalidReaction", err)
	}

	if _, err := f.s.HandleTransportRequestForConnection(context.Background(), f.reg("guest"), 8, "guest", TransportRequest{
		Action: TransportActionPause, PositionSeconds: new(100.0), Reaction: ReactionBRB,
	}); err != nil {
		t.Fatalf("pause with brb = %v", err)
	}
	command := f.s.rooms[f.repo.room.ID].command
	if command == nil || command.Action != TransportActionPause {
		t.Fatalf("room command = %+v; want the pause", command)
	}
	got := reactionsIn(f.conns["host"])
	if len(got) != 1 || got[0].Preset != ReactionBRB || got[0].CommandID != command.CommandID || got[0].From.DisplayName != "Asha" {
		t.Fatalf("host received %+v; want brb from Asha naming pause %s", got, command.CommandID)
	}
}

func TestPauseWithReactionNeedsTransportRights(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	if _, err := f.s.HandleTransportRequestForConnection(context.Background(), f.reg("guest"), 8, "guest", TransportRequest{
		Action: TransportActionPause, PositionSeconds: new(100.0), Reaction: ReactionBRB,
	}); !errors.Is(err, ErrTransportNotAllowed) {
		t.Fatalf("host-only guest pause error = %v; want ErrTransportNotAllowed", err)
	}
	if got := reactionsIn(f.conns["host"]); len(got) != 0 {
		t.Fatalf("a refused pause still sent %+v", got)
	}
}

// fanoutBus delivers every published event to every subscriber, as Redis
// PubSub does, including the publisher.
type fanoutBus struct {
	mu       sync.Mutex
	handlers []cache.EventHandler
}

func (b *fanoutBus) Publish(_ context.Context, _ string, event cache.Event) error {
	b.mu.Lock()
	handlers := append([]cache.EventHandler(nil), b.handlers...)
	b.mu.Unlock()
	for _, handler := range handlers {
		handler(event)
	}
	return nil
}

func (b *fanoutBus) Subscribe(_ context.Context, _ string, handler cache.EventHandler) error {
	b.mu.Lock()
	b.handlers = append(b.handlers, handler)
	b.mu.Unlock()
	return nil
}

func (b *fanoutBus) Close() error { return nil }

type signalConn struct{ received chan map[string]any }

func (c signalConn) WriteJSON(v any) error {
	if payload, ok := v.(map[string]any); ok {
		c.received <- payload
	}
	return nil
}
func (signalConn) Close() error { return nil }

func TestReactionsCrossServers(t *testing.T) {
	f := newBufferingRoom(t)
	remote := newServiceForTest(f.now, f.repo, nil, nil, nil)
	t.Cleanup(remote.Close)
	bus := new(fanoutBus)
	for _, s := range []*Service{f.s, remote} {
		if err := s.SetClusterEventBus(bus); err != nil {
			t.Fatal(err)
		}
	}
	// The guest's socket is on the remote server.
	guest := signalConn{received: make(chan map[string]any, 4)}
	remote.rooms[f.repo.room.ID].members[buildMemberKey(8, "guest")] = &memberState{userID: 8, profileID: "guest", connection: guest}
	f.member("host").displayName = "Ravi"

	if err := f.s.SendReactionForConnection(f.reg("host"), ReactionSnackBreak); err != nil {
		t.Fatalf("SendReactionForConnection = %v", err)
	}
	select {
	case payload := <-guest.received:
		reaction, _ := payload[reactionMessageType].(Reaction)
		if payload[clusterMessageTypeKey] != reactionMessageType || reaction.Preset != ReactionSnackBreak || reaction.From.DisplayName != "Ravi" {
			t.Fatalf("remote guest received %+v; want snack_break from Ravi", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reaction never reached the member on the other server")
	}
	// The sending server delivered to its own socket once, not again from the bus.
	if got := len(reactionsIn(f.conns["host"])); got != 1 {
		t.Fatalf("host received %d copies; want 1", got)
	}
}
