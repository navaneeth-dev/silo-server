package watchtogether

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-server/internal/cache"
)

// Reactions are preset messages ("brb", "back", ...) a member sends to the
// whole room. They are never stored: the server holding the sender's socket
// delivers them to its own sockets and publishes them on the playback event
// bus for the others, and a member who was not connected never sees them.
// Presets only, so every client, including one driven by a TV remote, can
// show and send all of them.

type ReactionPreset string

const (
	ReactionBRB        ReactionPreset = "brb"
	ReactionBack       ReactionPreset = "back"
	ReactionSnackBreak ReactionPreset = "snack_break"
	ReactionLaugh      ReactionPreset = "laugh"
)

func validReactionPreset(preset ReactionPreset) bool {
	switch preset {
	case ReactionBRB, ReactionBack, ReactionSnackBreak, ReactionLaugh:
		return true
	}
	return false
}

// Reaction is one delivered reaction.
type Reaction struct {
	ReactionID string         `json:"reaction_id"`
	Preset     ReactionPreset `json:"preset"`
	From       MemberIdentity `json:"from"`
	// CommandID names the pause the reaction was sent with, so clients can
	// show both as one notice.
	CommandID string `json:"command_id,omitempty"`
	SentAt    string `json:"sent_at"`
}

const (
	reactionMessageType      = "reaction"
	clusterReactionEventType = "watch_together_reaction"
	// Each member may send reactionBurst reactions at once, then one every
	// reactionInterval. The budget belongs to the server holding the member's
	// socket; a member has one socket at a time.
	reactionBurst    = 3
	reactionInterval = 2 * time.Second
)

var (
	ErrInvalidReaction     = errors.New("watch together reaction is not a known preset")
	ErrReactionRateLimited = errors.New("watch together reactions are rate limited")
)

type clusterReactionEvent struct {
	Source   string   `json:"source"`
	RoomID   string   `json:"room_id"`
	Reaction Reaction `json:"reaction"`
}

// SendReactionForConnection delivers preset from the member on reg to every
// connected member of the room, the sender included.
func (s *Service) SendReactionForConnection(reg *Registration, preset ReactionPreset) error {
	if s == nil || reg == nil {
		return ErrRoomForbidden
	}
	if !validReactionPreset(preset) {
		return ErrInvalidReaction
	}
	s.mu.Lock()
	live := s.rooms[reg.roomID]
	var member *memberState
	if live != nil && live.room.Phase != RoomPhaseEnded {
		member = live.members[reg.memberKey]
	}
	if member == nil || member.connection == nil || member.connection != reg.connection {
		s.mu.Unlock()
		return ErrRoomForbidden
	}
	if !s.allowReactionLocked(live, reg.memberKey) {
		s.mu.Unlock()
		return ErrReactionRateLimited
	}
	reaction := s.newReactionLocked(member, preset, "")
	dispatches := reactionDispatchesLocked(live, reaction)
	s.mu.Unlock()
	s.runDispatches(dispatches)
	s.publishReaction(reg.roomID, reaction)
	return nil
}

func (s *Service) allowReactionLocked(live *liveRoom, memberKey string) bool {
	if live.reactionLimiters == nil {
		live.reactionLimiters = make(map[string]*rate.Limiter)
	}
	limiter := live.reactionLimiters[memberKey]
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Every(reactionInterval), reactionBurst)
		live.reactionLimiters[memberKey] = limiter
	}
	return limiter.AllowN(s.now(), 1)
}

func (s *Service) newReactionLocked(member *memberState, preset ReactionPreset, commandID string) Reaction {
	return Reaction{
		ReactionID: uuid.NewString(), Preset: preset, From: *member.identity(),
		CommandID: commandID, SentAt: s.now().UTC().Format(time.RFC3339Nano),
	}
}

func reactionDispatchesLocked(live *liveRoom, reaction Reaction) []snapshotDispatch {
	dispatches := make([]snapshotDispatch, 0, len(live.members))
	for _, member := range live.members {
		if member == nil || member.connection == nil {
			continue
		}
		dispatches = append(dispatches, snapshotDispatch{conn: member.connection, payload: map[string]any{
			clusterMessageTypeKey: reactionMessageType, reactionMessageType: reaction,
		}})
	}
	return dispatches
}

func (s *Service) publishReaction(roomID string, reaction Reaction) {
	s.clusterMu.Lock()
	bus := s.clusterBus
	s.clusterMu.Unlock()
	if bus == nil {
		return
	}
	payload, err := json.Marshal(clusterReactionEvent{Source: s.instanceID, RoomID: roomID, Reaction: reaction})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bus.Publish(ctx, cache.ChannelPlayback, cache.Event{Type: clusterReactionEventType, Payload: string(payload)})
	}()
}

// handleClusterReaction delivers another server's reaction to this server's
// sockets in the room.
func (s *Service) handleClusterReaction(payload string) {
	var incoming clusterReactionEvent
	if json.Unmarshal([]byte(payload), &incoming) != nil || incoming.Source == s.instanceID || incoming.RoomID == "" ||
		!validReactionPreset(incoming.Reaction.Preset) {
		return
	}
	s.mu.Lock()
	live := s.rooms[incoming.RoomID]
	if live == nil || live.room.Phase == RoomPhaseEnded {
		s.mu.Unlock()
		return
	}
	dispatches := reactionDispatchesLocked(live, incoming.Reaction)
	s.mu.Unlock()
	s.runDispatches(dispatches)
}
