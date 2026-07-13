package dcg

// BoardTracker reconstructs the live board from the trace stream: every card
// object's zone (hand/deck/lanes/graveyard/fountain/…), combat target, and
// status flags, driven by the CTraceMsg_* movement/combat events. A snapshot is
// emitted into the packets log at each turn end.

import "sort"

// BoardCard is one card in a board snapshot.
type BoardCard struct {
	ObjectID uint64 `json:"objectId"`
	CardID   uint32 `json:"cardId,omitempty"`
	CardName string `json:"cardName,omitempty"`
	// Position is the combat slot within a lane (sorted ascending).
	Position int `json:"position,omitempty"`
	// AttackTarget is the object id this unit currently attacks; the arrow
	// direction (0 straight/tower, 1..) qualifies it.
	AttackTarget   uint64  `json:"attackTarget,omitempty"`
	ArrowDirection *uint64 `json:"arrowDirection,omitempty"`
	// ControlledBy is the object id of the card that owns this one when the
	// owner is another game object (an improvement/hero that spawned it) rather
	// than a player. The card is still grouped under that object's real player.
	ControlledBy uint64         `json:"controlledBy,omitempty"`
	Flags        map[string]any `json:"flags,omitempty"`
}

// PlayerBoard is one player's side of the snapshot. Player 0 and 1 are the two
// contestants; player 2 is the server-side staging owner (neutral units being
// prepared for assignment).
type PlayerBoard struct {
	Player    uint64          `json:"player"`
	Hand      []*BoardCard    `json:"hand"`
	ItemDeck  []*BoardCard    `json:"itemDeck,omitempty"`
	Graveyard []*BoardCard    `json:"graveyard"`
	Fountain  []*BoardCard    `json:"fountain"`
	Lanes     [3][]*BoardCard `json:"lanes"`
	// Other holds transient zones (spell stack, effects, shop, unresolved).
	Other map[string][]*BoardCard `json:"other,omitempty"`
}

// BoardSnapshot is a full board snapshot at a turn boundary.
type BoardSnapshot struct {
	Message string         `json:"message"` // "boardState"
	Turn    int            `json:"turn"`
	Players []*PlayerBoard `json:"players"`
}

// trackedCard is the tracker's mutable per-object state.
type trackedCard struct {
	objectID  uint64
	cardID    uint32
	owner     uint64 // a player id (0/1/2) or, for spawned units, the owning object id
	hasOwner  bool
	zone      string // deck|hand|lane|graveyard|fountain|spellStack|effects|shop|itemDeck
	lane      int    // 1..3 when zone == "lane"
	position  int
	target    uint64
	arrow     *uint64
	isHero    bool // heroes visit the fountain during match setup
	deathTurn int  // turn a hero entered the fountain by dying
	flags     map[string]any
}

// BoardTracker applies traces and produces snapshots.
type BoardTracker struct {
	cards map[uint64]*trackedCard
	turn  int
}

// NewBoardTracker returns an empty tracker.
func NewBoardTracker() *BoardTracker {
	return &BoardTracker{cards: map[uint64]*trackedCard{}}
}

func (bt *BoardTracker) get(id uint64) *trackedCard {
	c := bt.cards[id]
	if c == nil {
		c = &trackedCard{objectID: id, zone: "deck"}
		bt.cards[id] = c
	}
	return c
}

func (bt *BoardTracker) setFlag(c *trackedCard, k string, v any) {
	if c.flags == nil {
		c.flags = map[string]any{}
	}
	c.flags[k] = v
}

// SetTurn tells the tracker the current turn (for respawn countdowns).
func (bt *BoardTracker) SetTurn(turn int) { bt.turn = turn }

// Learn backfills card identity from a decoded board object (full states carry
// objects whose CardCreated trace was delta-mangled).
func (bt *BoardTracker) Learn(ob Object) {
	c := bt.get(ob.ObjectID)
	if c.cardID == 0 && ob.TemplateCardID != 0 {
		c.cardID = ob.TemplateCardID
	}
	if !c.hasOwner && ob.OwnerID != nil {
		c.owner, c.hasOwner = uint64(*ob.OwnerID), true
	}
	// Full states carry each object's authoritative zone/lane/position; use them
	// to place cards the trace stream never explicitly moved (e.g. cards sitting
	// in hand, which have no draw trace in some replays).
	for _, at := range ob.Attributes {
		switch at.Name {
		case "zoneId":
			if z, lane, ok := gameZone(at.Value); ok {
				c.zone = z
				if lane != 0 {
					c.lane = lane
				}
			}
		case "laneId":
			if at.Value >= 1 && at.Value <= 3 {
				c.lane = int(at.Value)
			}
		case "combatPosition":
			c.position = int(at.Value)
		}
	}
}

// Apply advances the board by one trace event.
func (bt *BoardTracker) Apply(tr Trace) {
	if tr.ObjectID == 0 {
		return
	}
	f := func(name string) (uint64, bool) { return toU64(tr.Fields[name]) }
	switch tr.Type {
	case "CardCreated", "RandomShopItemCreated":
		c := bt.get(tr.ObjectID)
		if v, ok := f("template_card_id"); ok {
			c.cardID = uint32(v)
		}
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
		if v, ok := f("is_item"); ok && v != 0 {
			c.zone = "itemDeck"
		}
		// Place the card from its instance attributes (1.0 card records carry
		// zoneId/ownerId/laneId in card_instance_attributes).
		if a, ok := tr.Fields["card_instance_attributes"].(map[string]any); ok {
			bt.placeFromAttrs(c, a)
		}
	case "CardRevealed":
		c := bt.get(tr.ObjectID)
		if v, ok := f("template_card_id"); ok {
			c.cardID = uint32(v)
		}
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
		bt.setFlag(c, "revealed", true)
	case "CardDrawn", "CardMovedToHand", "CardBought":
		c := bt.get(tr.ObjectID)
		c.zone = "hand"
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
	case "CardMovedToDeck":
		c := bt.get(tr.ObjectID)
		c.zone = "deck"
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
	case "CardMovedToDeployment":
		c := bt.get(tr.ObjectID)
		c.zone = "lane"
		if v, ok := f("lane_id"); ok {
			c.lane = int(v)
		}
	case "UnitEnteringCombat":
		c := bt.get(tr.ObjectID)
		c.zone = "lane"
		if v, ok := f("new_lane_id"); ok {
			c.lane = int(v)
		}
		if v, ok := f("new_combat_position"); ok {
			c.position = int(v)
		}
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
	case "UnitChangedCombatPosition":
		c := bt.get(tr.ObjectID)
		if v, ok := f("new_combat_position"); ok {
			c.position = int(v)
		}
		if v, ok := f("lane_id"); ok && v >= 1 && v <= 3 {
			c.zone, c.lane = "lane", int(v)
		}
	case "UnitChangedCombatTarget":
		c := bt.get(tr.ObjectID)
		if v, ok := f("new_combat_target"); ok {
			c.target = v
		}
		if v, ok := f("arrow_direction"); ok {
			c.arrow = &v
		}
	case "UnitLeavingCombat":
		c := bt.get(tr.ObjectID)
		// The trace names the destination zone directly (graveyard when a unit
		// dies out of combat, fountain when a hero returns, improvements, …).
		if v, ok := f("new_zone_id"); ok {
			if z, lane, ok := gameZone(v); ok {
				c.zone = z
				if lane != 0 {
					c.lane = lane
				}
			}
		} else if c.zone == "lane" {
			c.zone = "other" // destination arrives with the following move trace
		}
	case "CardMovedToGraveyard", "CardDiscarded", "ImprovementDestroyed":
		bt.get(tr.ObjectID).zone = "graveyard"
	case "CardKilled":
		c := bt.get(tr.ObjectID)
		if hero, ok := f("is_hero"); (ok && hero != 0) || c.isHero {
			c.zone = "fountain" // heroes respawn via the fountain
			c.deathTurn = bt.turn
		} else {
			c.zone = "graveyard"
		}
		c.target, c.arrow = 0, nil
	case "DamageAttempted":
		// CardKilled records often lose their subject id in deltas, but the
		// damage trace keeps it: zero remaining health on a non-tower is death.
		if rem, ok := f("remaining_health"); ok && rem == 0 {
			if tow, _ := f("is_tower"); tow == 0 {
				c := bt.get(tr.ObjectID)
				if c.isHero {
					c.zone = "fountain"
					c.deathTurn = bt.turn
				} else {
					c.zone = "graveyard"
				}
				c.target, c.arrow = 0, nil
			}
		}
	case "CardReturnedToFountain":
		c := bt.get(tr.ObjectID)
		c.zone = "fountain"
		c.isHero = true // only heroes pass through the fountain
	case "CardMovedToSpellStack":
		bt.get(tr.ObjectID).zone = "spellStack"
	case "CardMovedToEffects", "ImprovementEnteringPlay":
		c := bt.get(tr.ObjectID)
		c.zone = "effects"
		if v, ok := f("new_lane_id"); ok {
			c.lane = int(v)
		}
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = v, true
		}
	case "ShoppingCardAppeared":
		bt.get(tr.ObjectID).zone = "shop"
	case "UnitLocked":
		c := bt.get(tr.ObjectID)
		if v, ok := f("turns"); ok {
			bt.setFlag(c, "locked", v)
		}
	case "UnitCondemned":
		bt.setFlag(bt.get(tr.ObjectID), "condemned", true)
	case "ItemEquipped":
		c := bt.get(tr.ObjectID)
		if v, ok := f("hero_equipped_to_id"); ok {
			bt.setFlag(c, "equippedTo", v)
		}
		c.zone = "effects"
	}
}

// Snapshot renders the current board, grouped per owner and zone. turn labels
// the snapshot.
func (bt *BoardTracker) Snapshot(turn int) *BoardSnapshot {
	byPlayer := map[uint64]*PlayerBoard{}
	pb := func(owner uint64) *PlayerBoard {
		p := byPlayer[owner]
		if p == nil {
			p = &PlayerBoard{
				Player:    owner,
				Hand:      []*BoardCard{},
				Graveyard: []*BoardCard{}, Fountain: []*BoardCard{},
			}
			for i := range p.Lanes {
				p.Lanes[i] = []*BoardCard{}
			}
			byPlayer[owner] = p
		}
		return p
	}
	for _, c := range bt.cards {
		if c.cardID == 0 && c.zone == "deck" {
			continue // control objects never materialised as cards
		}
		if isPathMarker(c.cardID) {
			continue // 1002..1005 are attack-direction target markers, not units
		}
		bc := &BoardCard{
			ObjectID: c.objectID, CardID: c.cardID, CardName: CardName(c.cardID),
			Position: c.position, AttackTarget: c.target, ArrowDirection: c.arrow,
			Flags: c.flags,
		}
		if c.zone == "fountain" && c.deathTurn > 0 {
			// A dead hero redeploys after sitting out one full turn.
			respawnIn := c.deathTurn + 2 - turn
			if respawnIn < 0 {
				respawnIn = 0
			}
			if bc.Flags == nil {
				bc.Flags = map[string]any{}
			}
			bc.Flags["respawnIn"] = respawnIn
		}
		// Resolve the owner to a real player. Spawned units carry the objectId of
		// the card that created them (an improvement/hero) as their owner; follow
		// that chain to the controlling player and record the immediate owner.
		player, controller := bt.resolveOwner(c.owner)
		bc.ControlledBy = controller
		p := pb(player)
		switch c.zone {
		case "hand":
			p.Hand = append(p.Hand, bc)
		case "deck":
			// deck contents are in the demo-info decks; omit from snapshots
		case "itemDeck":
			p.ItemDeck = append(p.ItemDeck, bc)
		case "graveyard":
			p.Graveyard = append(p.Graveyard, bc)
		case "fountain":
			p.Fountain = append(p.Fountain, bc)
		case "lane":
			if c.lane >= 1 && c.lane <= 3 {
				p.Lanes[c.lane-1] = append(p.Lanes[c.lane-1], bc)
			} else {
				addOther(p, "lane?", bc)
			}
		default:
			addOther(p, c.zone, bc)
		}
	}
	st := &BoardSnapshot{Message: "boardState", Turn: turn}
	for _, p := range byPlayer {
		for i := range p.Lanes {
			lane := p.Lanes[i]
			sort.Slice(lane, func(a, b int) bool { return lane[a].Position < lane[b].Position })
		}
		sortCards(p.Hand)
		sortCards(p.Graveyard)
		sortCards(p.Fountain)
		st.Players = append(st.Players, p)
	}
	sort.Slice(st.Players, func(a, b int) bool { return st.Players[a].Player < st.Players[b].Player })
	return st
}

// maxPlayerID bounds real player ids: 0 and 1 are the contestants, 2 is the
// server staging owner. Anything larger is an object id (a spawning card).
const maxPlayerID = 2

// resolveOwner walks an owner up to a real player. When a card's owner is the
// object id of the card that spawned it, the chain is followed to that card's
// player; controller is the immediate object owner (0 when the owner was
// already a player). Cycles and dangling ids are bounded/tolerated.
func (bt *BoardTracker) resolveOwner(owner uint64) (player, controller uint64) {
	o := owner
	for i := 0; o > maxPlayerID && i < 8; i++ {
		ctrl := bt.cards[o]
		if ctrl == nil || !ctrl.hasOwner || ctrl.owner == o {
			break
		}
		controller = o
		o = ctrl.owner
	}
	return o, controller
}

// isPathMarker reports whether a card id is one of the attack-direction target
// markers (1002 left, 1003 forward, 1004 right, 1005 forward-2) the engine
// spawns for combat targeting rather than a real unit.
func isPathMarker(cardID uint32) bool { return cardID >= 1002 && cardID <= 1005 }

func addOther(p *PlayerBoard, zone string, bc *BoardCard) {
	if p.Other == nil {
		p.Other = map[string][]*BoardCard{}
	}
	p.Other[zone] = append(p.Other[zone], bc)
}

func sortCards(cs []*BoardCard) {
	sort.Slice(cs, func(a, b int) bool { return cs[a].ObjectID < cs[b].ObjectID })
}

// gameZone maps the game's EGameZone (DCG_ZONE_*) enum to a board zone. Values
// are the declaration order of the DCG_ZONE_* strings in libclient (INVALID=0,
// GRAVEYARD=1, DECK=2, HAND=3, FOUNTAIN=4, DEPLOYMENT=5, COMBAT=6,
// SPELL_STACK=7, SHOP=8, EFFECTS=9, IMPROVEMENTS=10, LIFEBOAT=11), verified
// against the trace data (a unit leaving combat reports new_zone_id=1=graveyard
// / 4=fountain). DEPLOYMENT and COMBAT both place a unit in a lane, with the
// lane number carried by laneId. Returns ok=false for INVALID/unknown so the
// card keeps its current zone.
func gameZone(id uint64) (zone string, lane int, ok bool) {
	switch id {
	case 1:
		return "graveyard", 0, true
	case 2:
		return "deck", 0, true
	case 3:
		return "hand", 0, true
	case 4:
		return "fountain", 0, true
	case 5, 6:
		return "lane", 0, true // deployment / combat; lane from laneId
	case 7:
		return "spellStack", 0, true
	case 8:
		return "shop", 0, true
	case 9, 10:
		return "effects", 0, true // effects / improvements
	case 11:
		return "fountain", 0, true // lifeboat ~ holding area
	}
	return "", 0, false
}

// placeFromAttrs sets a card's zone/owner/position from its instance attributes.
func (bt *BoardTracker) placeFromAttrs(c *trackedCard, a map[string]any) {
	if v, ok := toU64(a["ownerId"]); ok {
		c.owner, c.hasOwner = v, true
	}
	if v, ok := toU64(a["zoneId"]); ok {
		if z, lane, ok := gameZone(v); ok {
			c.zone = z
			if lane != 0 {
				c.lane = lane
			}
		}
	}
	if v, ok := toU64(a["laneId"]); ok && v >= 1 && v <= 3 {
		c.lane = int(v)
	}
	if v, ok := toU64(a["combatPosition"]); ok {
		c.position = int(v)
	}
}
