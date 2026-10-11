// Package leveldat reads and writes a Bedrock world's level.dat: an 8-byte
// header followed by little-endian NBT. Fields the Data struct does not know
// are kept and written back unchanged.
package leveldat
