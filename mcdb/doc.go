// Package mcdb reads and writes Minecraft: Bedrock Edition worlds: the LevelDB
// database in a world's db folder, its level.dat and levelname.txt. Chunks
// are encoded with the chunk package; Helix's leveldb package stores them
// with Bedrock's compression, so BDS and the game can open the result.
//
// The types here (ChunkPos, Dimension) describe the file format only. They
// carry no engine behaviour.
package mcdb
