package dcg

// Source 2 string tokens. The board-state serialization keys every field and
// type by a 32-bit hash of the lowercased name — Valve's MakeStringToken, which
// is murmur2 with seed 0x31415926. We can therefore reverse the hashes by
// hashing a dictionary of candidate names (proto fields/types + known DCG
// attribute names) and looking the symbols up.
//
// Verified: MakeStringToken("objectid") == 0x7c48d532 (the `32 d5 48 7c` symbol),
// ("ownerid") == 0xa3c734e7, ("zoneid") == 0x3825eb34.

import "strings"

// MakeStringToken returns Valve's 32-bit token for name (case-insensitive).
func MakeStringToken(name string) uint32 {
	return murmur2(strings.ToLower(name), 0x31415926)
}

func murmur2(s string, seed uint32) uint32 {
	const m = 0x5bd1e995
	const r = 24
	data := []byte(s)
	l := len(data)
	h := seed ^ uint32(l)
	i := 0
	for l >= 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
		i += 4
		l -= 4
	}
	switch l {
	case 3:
		h ^= uint32(data[i+2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[i+1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[i])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}

// tokenNames is the reverse table token -> name, built from tokenWordlist.
var tokenNames = buildTokenNames()

// TokenName returns the decoded name for a token, or "" if unknown.
func TokenName(tok uint32) string { return tokenNames[tok] }

func buildTokenNames() map[uint32]string {
	m := make(map[uint32]string, len(tokenWordlist)*2)
	add := func(name string) {
		if name == "" {
			return
		}
		t := MakeStringToken(name)
		if _, ok := m[t]; !ok {
			m[t] = name
		}
	}
	for _, name := range tokenWordlist {
		add(name)
		// also try a camelCase variant of snake_case names and vice versa
		if strings.Contains(name, "_") {
			add(snakeToCamel(name))
		}
	}
	return m
}

func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) == 1 {
		return s
	}
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// tokenWordlist is the dictionary of candidate field/type/zone names. It is
// grown empirically (TestTokenCoverage reports unmatched symbols seen in the
// sample payloads). murmur is case-insensitive here so a single spelling per
// concept suffices, but both snake and camel forms are added for proto names.
var tokenWordlist = []string{
	// object identity / classification
	"type", "objectId", "ownerId", "zoneId", "laneId", "cardId", "templateId",
	"templateCardId", "instanceId", "cardInstanceId", "handle", "entityId",
	// CTraceMsg_CardCreated / CardRevealed fields
	"wasSpawnedAsDire", "isItem", "spawnDuplicateIndex", "isDuplicate",
	"cardInstanceAttributes", "cardCalculatedEffects", "owner_id", "template_card_id",
	// stats / attributes
	"health", "maxHealth", "currentHealth", "remainingHealth", "mana", "maxMana",
	"attack", "armor", "gold", "charges", "maxCharges", "manaCost", "goldCost",
	"siege", "retaliate", "cleave", "bounty", "level",
	// position / location
	"combatPosition", "combatTarget", "position", "arrowDirection", "slot",
	"handPosition", "deckPosition", "shopOrder", "lane", "zone", "deploymentOrder",
	// colors
	"color", "colors", "colorFlags", "colorsPlayable",
	// trace / event fields (from CTraceMsg_*)
	"sourceObjectId", "sourceCardId", "effectSourceCardId", "relatedObjectId",
	"targetObjectId", "targetCardId", "damageAttempted", "damageDealt",
	"damagePrevented", "damageType", "heal", "amount", "available", "value",
	"attrToken", "valueDiff", "valueFinal", "scriptType", "blockId", "eventName",
	"newCardId", "newCharges", "chargeDelta", "survivalChance", "turns", "manaSpent",
	"manaAmountRestored", "isCombat", "isHero", "isCreep", "isTower", "isDefeatForOwner",
	"newLaneId", "newCombatPosition", "newCombatTarget", "newZoneId", "previousZoneId",
	"newImprovementPosition", "isValidPathTarget", "isBoundaryPath", "newCardId",
	"passToCombat", "playerId", "hasWon", "heroEquippedToId", "copiedCardObjectId",
	"sourceEffectCardId", "targetGroup", "targetGroupNumber", "oldCombatPosition",
	"oldArrowDirection", "oldCombatTarget", "manaAmountRestoreAttempt",
	// container / message names
	"decklist", "deckData", "combatLogEntry", "data", "payload", "card", "cards",
	// zones (also used as enum names)
	"deck", "hand", "fountain", "graveyard", "shop", "deployment", "effects",
	"spellStack", "guardDeck", "combat", "itemDeck",
}
