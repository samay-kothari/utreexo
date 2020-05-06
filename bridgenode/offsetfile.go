package bridgenode

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"sync"

	"github.com/mit-dci/utreexo/util"
)

// buildOffsetFile builds an offsetFile which acts as an index
// for block locations since blk*.dat files generated by Bitcoin Core
// has blocks out of order.
//
// If you have more blk*.dat files to generate an index for, just
// delete the current offsetfile directory and run genproofs again.
// Fairly quick process with one blk*.dat file taking a few seconds.
//
// Returns the last block height that it processed.
func buildOffsetFile(tip util.Hash) (int32, error) {

	// Map to store Block Header Hashes for sorting purposes
	// blk*.dat files aren't in block order so this is needed
	nextMap := make(map[[32]byte]util.RawHeaderData)

	offsetFile, err := os.OpenFile(util.OffsetFilePath,
		os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}

	var lastOffsetHeight int32

	defer offsetFile.Close()
	for fileNum := 0; ; fileNum++ {
		fileName := fmt.Sprintf("blk%05d.dat", fileNum)
		fmt.Printf("Building offsetfile... %s\n", fileName)

		_, err := os.Stat(fileName)
		if os.IsNotExist(err) {
			fmt.Printf("%s doesn't exist; done building\n", fileName)
			break
		}
		// grab headers from the .dat file as RawHeaderData type
		rawheaders, err := readRawHeadersFromFile(uint32(fileNum))
		if err != nil {
			panic(err)
		}
		tip, lastOffsetHeight, err = writeBlockOffset(
			rawheaders, nextMap, offsetFile, lastOffsetHeight, tip)
		if err != nil {
			panic(err)
		}
	}

	// write the last height of the offsetfile
	// needed info for the main genproofs processes
	LastIndexOffsetHeightFile, err := os.OpenFile(
		util.LastIndexOffsetHeightFilePath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	LastIndexOffsetHeightFile.Write(util.U32tB(uint32(lastOffsetHeight))[:])
	LastIndexOffsetHeightFile.Close()

	return lastOffsetHeight, nil
}

/*
Proof file format is somewhat like the blk.dat and rev.dat files.  But it's
always in order!  The offset file is in 8 byte chunks, so to find the proof
data for block 100 (really 101), seek to byte 800 and read 8 bytes.

The proof file is: 4 bytes empty (zeros for now, could do something else later)
4 bytes proof length, then the proof data.

Offset file is: 8 byte int64 offset.  Right now it's all 1 big file, can
change to 4 byte which file and 4 byte offset within file like the blk/rev but
we're not running on fat32 so works OK for now.
*/

// pFileWorker takes in blockproof and height information from the channel
// and writes to disk. MUST NOT have more than one worker as the proofs need to be
// in order
func proofWriterWorker(
	proofChan chan []byte, fileWait *sync.WaitGroup) {

	// for the pFile
	proofFile, err := os.OpenFile(
		util.PFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}

	offsetFile, err := os.OpenFile(
		util.POffsetFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		panic(err)
	}

	_, err = offsetFile.Seek(0, 2)
	if err != nil {
		panic(err)
	}

	proofFileLocation, err := proofFile.Seek(0, 2)
	if err != nil {
		panic(err)

	}
	// TODO: optimization - don't write anything to proof file for blocks with
	// no deletions (inputs).  Lots of em in testnet.  Not so many on mainnet
	// I guess.  But in testnet would save millions *8 bytes.
	for {
		pbytes := <-proofChan
		// write to offset file first
		err = binary.Write(offsetFile, binary.BigEndian, proofFileLocation)
		if err != nil {
			fmt.Printf(err.Error())
			return
		}

		// write to proof file
		// first write big endian proof size int64
		err = binary.Write(proofFile, binary.BigEndian, int64(len(pbytes)))
		if err != nil {
			fmt.Printf(err.Error())
			return
		}
		proofFileLocation += 8

		// then write the proof
		written, err := proofFile.Write(pbytes)
		if err != nil {
			fmt.Printf(err.Error())
			return
		}
		proofFileLocation += int64(written)

		fileWait.Done()
	}
}

// readRawHeadersFromFile reads only the headers from the given .dat file
func readRawHeadersFromFile(fileNum uint32) ([]util.RawHeaderData, error) {
	var blockHeaders []util.RawHeaderData

	fileName := fmt.Sprintf("blk%05d.dat", fileNum)
	f, err := os.Open(fileName)
	if err != nil {
		panic(err)
	}

	fStat, err := f.Stat()
	if err != nil {
		panic(err)
	}

	fSize := fStat.Size()

	defer f.Close()
	loc := int64(0)
	offset := uint32(0) // where the block is located from the beginning of the file

	// until offset is at the end of the file
	for loc != fSize {
		b := new(util.RawHeaderData)
		copy(b.FileNum[:], util.U32tB(fileNum))
		copy(b.Offset[:], util.U32tB(offset))

		// check if Bitcoin magic bytes were read
		var magicbytes [4]byte
		f.Read(magicbytes[:])
		if util.CheckMagicByte(magicbytes) == false {
			break
		}

		// read the 4 byte size of the load of the block
		var size [4]byte
		f.Read(size[:])

		// add 8bytes for the magic bytes (4bytes) and size (4bytes)
		offset = offset + util.LBtU32(size[:]) + uint32(8)

		var blockheader [80]byte
		f.Read(blockheader[:])

		copy(b.Prevhash[:], blockheader[4:32])

		// create block hash
		// double sha256 needed with Bitcoin
		first := sha256.Sum256(blockheader[:])
		b.CurrentHeaderHash = sha256.Sum256(first[:])

		// offset for the next block from the current position
		loc, err = f.Seek(int64(util.LBtU32(size[:]))-80, 1)
		if err != nil {
			return nil, err
		}
		blockHeaders = append(blockHeaders, *b)
		b = nil
	}
	return blockHeaders, nil
}

// Sorts and writes the block offset from the passed in blockHeaders.
func writeBlockOffset(
	blockHeaders []util.RawHeaderData, //        All headers from the select .dat file
	nextMap map[[32]byte]util.RawHeaderData, //  Map to save the current block hash
	offsetFile *os.File, //                 File to save the sorted blocks and locations to
	tipnum int32, //                          Current block it's on
	tip util.Hash) ( //                Current hash of the block it's on
	util.Hash, int32, error) {

	for _, b := range blockHeaders {
		if len(nextMap) > 10000 { //Just a random big number
			fmt.Println("Dead end tip. Exiting...")
			break
		}

		// The block's Prevhash doesn't match the
		// previous block header. Add to map.
		// Searches until it finds a hash that does.
		if b.Prevhash != tip {
			nextMap[b.Prevhash] = b
			continue
		}

		// Write the .dat file name and the
		// offset the block can be found at
		offsetFile.Write(b.FileNum[:])
		offsetFile.Write(b.Offset[:])

		// set the tip to current block's hash
		tip = b.CurrentHeaderHash
		tipnum++

		// check for next blocks in map
		// same thing but with the stored blocks
		// that we skipped over
		stashedBlock, ok := nextMap[tip]
		for ok {
			// Write the .dat file name and the
			// offset the block can be found at
			offsetFile.Write(stashedBlock.FileNum[:])
			offsetFile.Write(stashedBlock.Offset[:])

			// set the tip to current block's hash
			tip = stashedBlock.CurrentHeaderHash
			tipnum++

			// remove the written current block
			delete(nextMap, stashedBlock.Prevhash)

			// move to the next block
			stashedBlock, ok = nextMap[tip]
		}
	}
	return tip, tipnum, nil
}
