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
	AttackTarget   uint64         `json:"attackTarget,omitempty"`
	ArrowDirection *uint64        `json:"arrowDirection,omitempty"`
	Flags          map[string]any `json:"flags,omitempty"`
}

// PlayerBoard is one player's side of the snapshot.
type PlayerBoard struct {
	Player    uint32          `json:"player"`
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
	owner     uint32
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
		c.owner, c.hasOwner = *ob.OwnerID, true
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
			c.owner, c.hasOwner = uint32(v), true
		}
		if v, ok := f("is_item"); ok && v != 0 {
			c.zone = "itemDeck"
		}
	case "CardRevealed":
		c := bt.get(tr.ObjectID)
		if v, ok := f("template_card_id"); ok {
			c.cardID = uint32(v)
		}
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = uint32(v), true
		}
		bt.setFlag(c, "revealed", true)
	case "CardDrawn", "CardMovedToHand", "CardBought":
		c := bt.get(tr.ObjectID)
		c.zone = "hand"
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = uint32(v), true
		}
	case "CardMovedToDeck":
		c := bt.get(tr.ObjectID)
		c.zone = "deck"
		if v, ok := f("owner_id"); ok {
			c.owner, c.hasOwner = uint32(v), true
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
			c.owner, c.hasOwner = uint32(v), true
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
		if c.zone == "lane" {
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
			c.owner, c.hasOwner = uint32(v), true
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
	byPlayer := map[uint32]*PlayerBoard{}
	pb := func(owner uint32) *PlayerBoard {
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
		p := pb(c.owner)
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

func addOther(p *PlayerBoard, zone string, bc *BoardCard) {
	if p.Other == nil {
		p.Other = map[string][]*BoardCard{}
	}
	p.Other[zone] = append(p.Other[zone], bc)
}

func sortCards(cs []*BoardCard) {
	sort.Slice(cs, func(a, b int) bool { return cs[a].ObjectID < cs[b].ObjectID })
}
