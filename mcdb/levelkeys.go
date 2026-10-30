package mcdb

// Level keys of BDS 1.26.60: every world-wide key it reads, writes or scans
// when a world opens or is played, found by watching every storage call of
// a running BDS. Values are LE NBT unless noted; their contents belong to
// the systems that own them. Use them with LoadLevel, StoreLevel,
// DeleteLevel and ForEachLevel.
const (
	// LevelKeyOverworld, LevelKeyNether and LevelKeyTheEnd hold each
	// dimension's data ({data: {LimboEntities}}; the End adds DragonFight).
	LevelKeyOverworld = "Overworld"
	LevelKeyNether    = "Nether"
	LevelKeyTheEnd    = "TheEnd"
	// LevelKeyAutonomousEntities holds {AutonomousEntityList}.
	LevelKeyAutonomousEntities = "AutonomousEntities"
	// LevelKeyWorldClocks holds {clocks: [{IsPaused, Name, Time}]}.
	LevelKeyWorldClocks = "WorldClocks"
	// LevelKeyBiomeData holds {list: [{id, snowAccumulation}]}.
	LevelKeyBiomeData = "BiomeData"
	// LevelKeyBiomeIDsTable maps custom biome names to IDs.
	LevelKeyBiomeIDsTable = "BiomeIdsTable"
	// LevelKeyDimensionNameIDTable maps custom dimension names to IDs.
	LevelKeyDimensionNameIDTable = "DimensionNameIdTable"
	// LevelKeyMobEvents holds which mob events are enabled.
	LevelKeyMobEvents = "mobevents"
	// LevelKeyScoreboard holds objectives, entries and display slots.
	LevelKeyScoreboard = "scoreboard"
	// LevelKeyWanderingTraderScheduler holds the wandering trader schedule.
	LevelKeyWanderingTraderScheduler = "schedulerWT"
	// LevelKeyPortals holds the portal records.
	LevelKeyPortals = "portals"
	// LevelKeyDynamicProperties holds Script API dynamic properties.
	LevelKeyDynamicProperties = "DynamicProperties"
	// LevelKeyChunkMetaData is the chunk metadata dictionary (raw, not NBT:
	// uint32 count, then hash and NBT per entry). DB maintains it.
	LevelKeyChunkMetaData = keyChunkMetaData
	// LevelKeyForcedCorruption marks a world BDS was told to treat as
	// corrupt.
	LevelKeyForcedCorruption = "DedicatedServerForcedCorruption"
	// LevelKeyPositionTrackLastID holds the last lodestone tracking ID.
	LevelKeyPositionTrackLastID = "PositionTrackDB-LastId"
	// LevelKeySpawnWasFixed marks a world whose spawn was moved by a fix.
	LevelKeySpawnWasFixed = "LevelSpawnWasFixed"
	// LevelKeyLocalPlayer is the local player of a single-player world.
	LevelKeyLocalPlayer = "~local_player"
)

// Prefix families: BDS scans these with a prefix search when a world opens,
// or writes one key per object under them.
const (
	// LevelPrefixTickingArea + UUID: one ticking area each.
	LevelPrefixTickingArea = "tickingarea_"
	// LevelPrefixVillage + dimension + "_" + UUID + "_" + INFO, DWELLERS,
	// PLAYERS, POI or RAID.
	LevelPrefixVillage = "VILLAGE_"
	// LevelPrefixOwnedEntitiesLimbo + 8-byte ID: entities waiting for their
	// owner's chunk.
	LevelPrefixOwnedEntitiesLimbo = "OwnedEntitiesLimbo"
	// LevelPrefixGameTestInstance: game test instances.
	LevelPrefixGameTestInstance = "gametestinstance_"
	// LevelPrefixMap + map ID: one map item's data each.
	LevelPrefixMap = "map_"
	// LevelPrefixStructureTemplate + namespace:name: saved structures.
	LevelPrefixStructureTemplate = "structuretemplate_"
	// LevelPrefixPositionTrack + ID: one lodestone tracking record each.
	LevelPrefixPositionTrack = "PositionTrackDB-"
	// LevelPrefixPlayer and LevelPrefixPlayerServer: see PlayerStore.
	LevelPrefixPlayer       = playerLookupPrefix
	LevelPrefixPlayerServer = playerServerPrefix
	// LevelPrefixEditorPlayer and LevelPrefixEditorPlayerServer: the same
	// for Editor mode.
	LevelPrefixEditorPlayer       = "player_editor_"
	LevelPrefixEditorPlayerServer = "player_server_editor_"
)

// LevelPrefixChunkLoadedRequest returns the prefix of a dimension's chunk
// load requests (from /tickingarea preload and scripts).
func LevelPrefixChunkLoadedRequest(dim Dimension) string {
	return "chunk_loaded_request_" + dimensionName(dim) + "_"
}

// LevelPrefixDelayedAction returns the prefix of a dimension's delayed
// action requests.
func LevelPrefixDelayedAction(dim Dimension) string {
	return "delay_action_request_" + dimensionName(dim) + "_"
}

// LevelPrefixStructurePlacement returns the prefix of a dimension's
// structure placement records.
func LevelPrefixStructurePlacement(dim Dimension) string {
	return "structureplacement_" + dimensionName(dim) + "_"
}

// LevelPrefixVillageIn returns the prefix of a dimension's village keys.
func LevelPrefixVillageIn(dim Dimension) string {
	return LevelPrefixVillage + dimensionName(dim) + "_"
}
