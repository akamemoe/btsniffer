package core

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/marksamman/bencode"
)

const (
	perBlock        = 16384
	maxMetadataSize = perBlock * 1024
	extended        = 20
	extHandshake    = 0
)

var (
	errExtHeader    = errors.New("invalid extension header response")
	errInvalidPiece = errors.New("invalid piece response")
	errTimeout      = errors.New("time out")
)

var metaWirePool = sync.Pool{
	New: func() interface{} {
		return &MetaWire{}
	},
}

type MetaWire struct {
	InfoHash     string
	From         string
	PeerID       string
	Conn         *net.TCPConn
	Timeout      time.Duration
	MetaDataSize int
	UtMetadata   int
	NumOfPieces  int
	Pieces       [][]byte
	Err          error
}

func NewMetaWire(infohash string, from string, timeout time.Duration) *MetaWire {
	w := metaWirePool.Get().(*MetaWire)
	w.InfoHash = infohash
	w.From = from
	w.PeerID = string(RandBytes(20))
	w.Timeout = timeout
	w.Conn = nil
	w.Err = nil
	return w
}

func (mw *MetaWire) Fetch() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mw.Timeout)
	defer cancel()
	return mw.FetchCtx(ctx)
}

func (mw *MetaWire) FetchCtx(ctx context.Context) ([]byte, error) {
	mw.connect(ctx)
	mw.handshake(ctx)
	mw.onHandshake(ctx)
	mw.extHandshake(ctx)

	if mw.Err != nil {
		if mw.Conn != nil {
			mw.Conn.Close()
		}
		return nil, mw.Err
	}

	for {
		data, err := mw.next(ctx)
		if err != nil {
			return nil, err
		}

		if data[0] != extended {
			continue
		}

		if err := mw.onExtended(ctx, data[1], data[2:]); err != nil {
			return nil, err
		}

		if !mw.checkDone() {
			continue
		}

		m := bytes.Join(mw.Pieces, []byte(""))
		sum := sha1.Sum(m)
		if bytes.Equal(sum[:], []byte(mw.InfoHash)) {
			return m, nil
		}

		return nil, errors.New("metadata checksum mismatch")
	}
}

func (mw *MetaWire) connect(ctx context.Context) {
	conn, err := net.DialTimeout("tcp", mw.From, mw.Timeout)
	if err != nil {
		mw.Err = fmt.Errorf("connect to remote peer failed: %v", err)
		return
	}

	mw.Conn = conn.(*net.TCPConn)
}

func (mw *MetaWire) handshake(ctx context.Context) {
	if mw.Err != nil {
		return
	}

	select {
	case <-ctx.Done():
		mw.Err = errTimeout
		return
	default:
	}

	buf := bytes.NewBuffer(nil)
	buf.Write(mw.preHeader())
	buf.WriteString(mw.InfoHash)
	buf.WriteString(mw.PeerID)
	_, mw.Err = mw.Conn.Write(buf.Bytes())
}

func (mw *MetaWire) onHandshake(ctx context.Context) {
	if mw.Err != nil {
		return
	}

	select {
	case <-ctx.Done():
		mw.Err = errTimeout
		return
	default:
	}

	res, err := mw.Read(ctx, 68)
	if err != nil {
		mw.Err = err
		return
	}

	if !bytes.Equal(res[:20], mw.preHeader()[:20]) {
		mw.Err = errors.New("remote peer not supporting bittorrent protocol")
		return
	}

	if res[25]&0x10 != 0x10 {
		mw.Err = errors.New("remote peer not supporting extension protocol")
		return
	}

	if !bytes.Equal(res[28:48], []byte(mw.InfoHash)) {
		mw.Err = errors.New("invalid bittorrent header response")
		return
	}
}

func (mw *MetaWire) extHandshake(ctx context.Context) {
	if mw.Err != nil {
		return
	}

	select {
	case <-ctx.Done():
		mw.Err = errTimeout
		return
	default:
	}

	data := append([]byte{extended, extHandshake}, bencode.Encode(map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": 1,
		},
	})...)
	if err := mw.Write(ctx, data); err != nil {
		mw.Err = err
		return
	}
}

func (mw *MetaWire) onExtHandshake(ctx context.Context, payload []byte) error {
	select {
	case <-ctx.Done():
		return errTimeout
	default:
	}

	dict, err := bencode.Decode(bytes.NewBuffer(payload))
	if err != nil {
		return errExtHeader
	}

	metadataSize, ok := dict["metadata_size"].(int64)
	if !ok {
		return errExtHeader
	}

	if metadataSize > maxMetadataSize {
		return errors.New("metadata_size too long")
	}

	if metadataSize < 0 {
		return errors.New("negative metadata_size")
	}

	m, ok := dict["m"].(map[string]interface{})
	if !ok {
		return errExtHeader
	}

	utMetadata, ok := m["ut_metadata"].(int64)
	if !ok {
		return errExtHeader
	}

	mw.MetaDataSize = int(metadataSize)
	mw.UtMetadata = int(utMetadata)
	mw.NumOfPieces = mw.MetaDataSize / perBlock
	if mw.MetaDataSize%perBlock != 0 {
		mw.NumOfPieces++
	}
	mw.Pieces = make([][]byte, mw.NumOfPieces)

	for i := 0; i < mw.NumOfPieces; i++ {
		mw.requestPiece(ctx, i)
	}

	return nil
}

func (mw *MetaWire) requestPiece(ctx context.Context, i int) {
	buf := bytes.NewBuffer(nil)
	buf.WriteByte(byte(extended))
	buf.WriteByte(byte(mw.UtMetadata))
	buf.Write(bencode.Encode(map[string]interface{}{
		"msg_type": 0,
		"piece":    i,
	}))
	mw.Write(ctx, buf.Bytes())
}

func (mw *MetaWire) onExtended(ctx context.Context, ext byte, payload []byte) error {
	if ext == 0 {
		if err := mw.onExtHandshake(ctx, payload); err != nil {
			return err
		}
	} else {
		piece, index, err := mw.onPiece(ctx, payload)
		if err != nil {
			return err
		}
		mw.Pieces[index] = piece
	}
	return nil
}

func (mw *MetaWire) onPiece(ctx context.Context, payload []byte) ([]byte, int, error) {
	select {
	case <-ctx.Done():
		return nil, -1, errTimeout
	default:
	}

	trailerIndex := bytes.Index(payload, []byte("ee")) + 2
	if trailerIndex == 1 {
		return nil, 0, errInvalidPiece
	}

	dict, err := bencode.Decode(bytes.NewBuffer(payload[:trailerIndex]))
	if err != nil {
		return nil, 0, errInvalidPiece
	}

	pieceIndex, ok := dict["piece"].(int64)
	if !ok || int(pieceIndex) >= mw.NumOfPieces {
		return nil, 0, errInvalidPiece
	}

	msgType, ok := dict["msg_type"].(int64)
	if !ok || msgType != 1 {
		return nil, 0, errInvalidPiece
	}

	return payload[trailerIndex:], int(pieceIndex), nil
}

func (mw *MetaWire) checkDone() bool {
	for _, b := range mw.Pieces {
		if b == nil {
			return false
		}
	}
	return true
}

func (mw *MetaWire) preHeader() []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteByte(19)
	buf.WriteString("BitTorrent protocol")
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x01})
	return buf.Bytes()
}

func (mw *MetaWire) next(ctx context.Context) ([]byte, error) {
	data, err := mw.Read(ctx, 4)
	if err != nil {
		return nil, err
	}

	size := binary.BigEndian.Uint32(data)
	data, err = mw.Read(ctx, size)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (mw *MetaWire) Read(ctx context.Context, size uint32) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, errTimeout
	default:
	}

	buf := bytes.NewBuffer(nil)
	_, err := io.CopyN(buf, mw.Conn, int64(size))
	if err != nil {
		return nil, fmt.Errorf("read %d bytes message failed: %v", size, err)
	}

	return buf.Bytes(), nil
}

func (mw *MetaWire) Write(ctx context.Context, data []byte) error {
	select {
	case <-ctx.Done():
		return errTimeout
	default:
	}

	buf := bytes.NewBuffer(nil)
	length := int32(len(data))
	binary.Write(buf, binary.BigEndian, length)
	buf.Write(data)
	_, err := mw.Conn.Write(buf.Bytes())
	if err != nil {
		return fmt.Errorf("write message failed: %v", err)
	}

	return nil
}

func (mw *MetaWire) Free() {
	metaWirePool.Put(mw)
}
