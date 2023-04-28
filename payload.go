package main

import (
	"compress/bzip2"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"

	humanize "github.com/dustin/go-humanize"
	"github.com/golang/protobuf/proto"
	// xz "github.com/spencercw/go-xz"
	xz "github.com/smira/go-xz"
	// xz "github.com/ulikunitz/xz"
	"github.com/ssut/payload-dumper-go/chromeos_update_engine"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
)

type request struct {
	partition       *chromeos_update_engine.PartitionUpdate
	targetDirectory string
}

// Payload is a new format for the Android OTA/Firmware update files since Android Oreo
type Payload struct {
	Filename string

	file                 *os.File
	header               *payloadHeader
	deltaArchiveManifest *chromeos_update_engine.DeltaArchiveManifest
	signatures           *chromeos_update_engine.Signatures

	metadataSize int64
	dataOffset   int64
	initialized  bool

	requests chan *request
	workerWG sync.WaitGroup
	progress *mpb.Progress
}

const payloadHeaderMagic = "CrAU"
const brilloMajorPayloadVersion = 2
const blockSize = 4096

type payloadHeader struct {
	Version              uint64
	ManifestLen          uint64
	MetadataSignatureLen uint32
	Size                 uint64

	payload *Payload
}

func (ph *payloadHeader) ReadFromPayload() error {
	buf := make([]byte, 4)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	if string(buf) != payloadHeaderMagic {
		return fmt.Errorf("Invalid payload magic: %s", buf)
	}

	// Read Version
	buf = make([]byte, 8)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.Version = binary.BigEndian.Uint64(buf)
	//fmt.Printf("Payload Version: %d\n", ph.Version)

	if ph.Version != brilloMajorPayloadVersion {
		return fmt.Errorf("Unsupported payload version: %d", ph.Version)
	}

	// Read Manifest Len
	buf = make([]byte, 8)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.ManifestLen = binary.BigEndian.Uint64(buf)
	//fmt.Printf("Payload Manifest Length: %d\n", ph.ManifestLen)

	ph.Size = 24

	// Read Manifest Signature Length
	buf = make([]byte, 4)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.MetadataSignatureLen = binary.BigEndian.Uint32(buf)
	//fmt.Printf("Payload Manifest Signature Length: %d\n", ph.MetadataSignatureLen)

	return nil
}

// NewPayload creates a new Payload struct
func NewPayload(filename string) Payload {
	payload := Payload{
		Filename: filename,
	}

	return payload
}

// Open tries to open payload.bin file defined by Filename
func (p *Payload) Open() error {
	file, err := os.Open(p.Filename)
	if err != nil {
		return err
	}

	p.file = file
	return nil
}

func (p *Payload) readManifest() (*chromeos_update_engine.DeltaArchiveManifest, error) {
	buf := make([]byte, p.header.ManifestLen)
	if _, err := p.file.Read(buf); err != nil {
		return nil, err
	}
	deltaArchiveManifest := &chromeos_update_engine.DeltaArchiveManifest{}
	if err := proto.Unmarshal(buf, deltaArchiveManifest); err != nil {
		return nil, err
	}

	return deltaArchiveManifest, nil
}

func (p *Payload) readMetadataSignature() (*chromeos_update_engine.Signatures, error) {
	if _, err := p.file.Seek(int64(p.header.Size+p.header.ManifestLen), 0); err != nil {
		return nil, err
	}

	buf := make([]byte, p.header.MetadataSignatureLen)
	if _, err := p.file.Read(buf); err != nil {
		return nil, err
	}
	signatures := &chromeos_update_engine.Signatures{}
	if err := proto.Unmarshal(buf, signatures); err != nil {
		return nil, err
	}

	return signatures, nil
}

func (p *Payload) Init() error {
	// Read Header
	p.header = &payloadHeader{
		payload: p,
	}
	if err := p.header.ReadFromPayload(); err != nil {
		return err
	}

	// Read Manifest
	deltaArchiveManifest, err := p.readManifest()
	if err != nil {
		return err
	}
	p.deltaArchiveManifest = deltaArchiveManifest

	// Read Signatures
	signatures, err := p.readMetadataSignature()
	if err != nil {
		return err
	}
	p.signatures = signatures

	// Update sizes
	p.metadataSize = int64(p.header.Size + p.header.ManifestLen)
	p.dataOffset = p.metadataSize + int64(p.header.MetadataSignatureLen)

	fmt.Println("Found partitions:")
	for i, partition := range p.deltaArchiveManifest.Partitions {
		fmt.Printf("%s (%s)", partition.GetPartitionName(), humanize.Bytes(*partition.GetNewPartitionInfo().Size))

		if i < len(deltaArchiveManifest.Partitions)-1 {
			fmt.Printf(", ")
		} else {
			fmt.Printf("\n")
		}
	}
	p.initialized = true

	return nil
}

func (p *Payload) readDataBlob(offset int64, length int64) ([]byte, error) {
	buf := make([]byte, length)
	n, err := p.file.ReadAt(buf, p.dataOffset+offset)
	if err != nil {
		return nil, err
	}
	if int64(n) != length {
		return nil, fmt.Errorf("Read length mismatch: %d != %d", n, length)
	}

	return buf, nil
}

func (p *Payload) Extract(partition *chromeos_update_engine.PartitionUpdate, out *os.File) error {
	name := partition.GetPartitionName()
	info := partition.GetNewPartitionInfo()
	totalOperations := len(partition.Operations)
	barName := fmt.Sprintf("%s (%s)", name, humanize.Bytes(info.GetSize()))
	bar := p.progress.AddBar(
		int64(totalOperations),
		mpb.PrependDecorators(
			decor.Name(barName, decor.WCSyncSpaceR),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
		),
	)
	defer bar.SetTotal(0, true)

	for _, operation := range partition.Operations {
		if len(operation.DstExtents) == 0 {
			return fmt.Errorf("Invalid operation.DstExtents for the partition %s", name)
		}
		bar.Increment()

		e := operation.DstExtents[0]
		dataOffset := p.dataOffset + int64(operation.GetDataOffset())
		dataLength := int64(operation.GetDataLength())
		_, err := out.Seek(int64(e.GetStartBlock())*blockSize, 0)
		if err != nil {
			return err
		}
		expectedUncompressedBlockSize := int64(e.GetNumBlocks() * blockSize)

		bufSha := sha256.New()
		teeReader := io.TeeReader(io.NewSectionReader(p.file, dataOffset, dataLength), bufSha)

		switch operation.GetType() {
		case chromeos_update_engine.InstallOperation_REPLACE:
			n, err := io.Copy(out, teeReader)
			if err != nil {
				return err
			}

			if int64(n) != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		case chromeos_update_engine.InstallOperation_REPLACE_XZ:
			reader, err := xz.NewReader(teeReader)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, reader)
			if err != nil {
				return err
			}
			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}

			break

		case chromeos_update_engine.InstallOperation_REPLACE_BZ:
			reader := bzip2.NewReader(teeReader)
			n, err := io.Copy(out, reader)
			if err != nil {
				return err
			}
			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		default:
			return fmt.Errorf("Unhandled operation type: %s", operation.GetType().String())
		}

		// verify hash
		hash := hex.EncodeToString(bufSha.Sum(nil))
		expectedHash := hex.EncodeToString(operation.GetDataSha256Hash())
		if hash != expectedHash {
			return fmt.Errorf("Verify failed (Checksum mismatch): %s (%s != %s)", name, hash, expectedHash)
		}
	}

	return nil
}

func (p *Payload) worker() {
	for req := range p.requests {
		partition := req.partition
		targetDirectory := req.targetDirectory

		name := fmt.Sprintf("%s.img", partition.GetPartitionName())
		filepath := fmt.Sprintf("%s/%s", targetDirectory, name)
		file, err := os.OpenFile(filepath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0755)
		if err != nil {
		}
		if err := p.Extract(partition, file); err != nil {
		}

		p.workerWG.Done()
	}
}

func (p *Payload) spawnExtractWorkers(n int) {
	for i := 0; i < n; i++ {
		go p.worker()
	}
}

func (p *Payload) ExtractSelected(targetDirectory string, partitions []string) error {

	if !p.initialized {
		return errors.New("Payload has not been initialized")
	}
	p.progress = mpb.New()

	p.requests = make(chan *request, 100)
	p.spawnExtractWorkers(4)

	sort.Strings(partitions)

	all_partitions := make(map[string]int)

	for index, partition := range p.deltaArchiveManifest.Partitions {
		all_partitions[partition.GetPartitionName()] = index
	}

	_, ok := all_partitions[partitions[0]]

	dump_flag := false

	for _, partition := range p.deltaArchiveManifest.Partitions {
		if len(partitions) > 0 {
			idx := sort.SearchStrings(partitions, *partition.PartitionName)
			if idx == len(partitions) || partitions[idx] != *partition.PartitionName {
				continue

			}
		}

		if ok {
			dump_flag = true
			if exists, _ := PathExists(targetDirectory); exists == false {
				if err := os.Mkdir(targetDirectory, 0755); err != nil {
					log.Fatal("Failed to create target directory")
				}
			}
			fmt.Println("\n+++++++++++ extra dir: ",targetDirectory  ,"+++++++++++\n")
			//fmt.Printf("\n")
			p.workerWG.Add(1)
			p.requests <- &request{
				partition:       partition,
				targetDirectory: targetDirectory,
			}
		}
	}

	p.workerWG.Wait()
	close(p.requests)

	if dump_flag {
		return nil
	} else {
		return errors.New("Can't find target partition")
	}

}

func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (p *Payload) DefaultExtract(targetDirectory string, partitions []string) error {

	if exists, _ := PathExists(targetDirectory); exists == false {
		if err := os.Mkdir(targetDirectory, 0755); err != nil {
			log.Fatal("Failed to create target directory")
		}
	}
	if !p.initialized {
		return errors.New("Payload has not been initialized")
	}
	p.progress = mpb.New()

	p.requests = make(chan *request, 100)
	p.spawnExtractWorkers(4)

	sort.Strings(partitions)

	for _, partition := range p.deltaArchiveManifest.Partitions {
		if len(partitions) > 0 {
			idx := sort.SearchStrings(partitions, *partition.PartitionName)
			if idx == len(partitions) || partitions[idx] != *partition.PartitionName {
				continue
			}
		}

		p.workerWG.Add(1)
		p.requests <- &request{
			partition:       partition,
			targetDirectory: targetDirectory,
		}
	}

	p.workerWG.Wait()
	close(p.requests)

	return nil
}

func (p *Payload) ExtractAll(targetDirectory string) error {
	return p.DefaultExtract(targetDirectory, nil)
}
