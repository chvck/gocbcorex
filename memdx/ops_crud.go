package memdx

import (
	"encoding/binary"
	"errors"
	"time"
)

type OpsCrud struct {
	ExtFramesEnabled      bool
	CollectionsEnabled    bool
	DurabilityEnabled     bool
	PreserveExpiryEnabled bool
}

func (o OpsCrud) encodeCollectionAndKey(collectionID uint32, key []byte, buf []byte) ([]byte, error) {
	if !o.CollectionsEnabled {
		if collectionID != 0 {
			return nil, ErrCollectionsNotEnabled
		}

		// we intentionally copy to the buffer here so that key does not escape
		buf = append(buf, key...)
		return buf, nil
	}

	return AppendCollectionIDAndKey(collectionID, key, buf)
}

// TODO(brett19): This exists in OpsUtils too, we should probably deduplicate the implementation.
func (o OpsCrud) encodeReqExtFrames(
	onBehalfOf string,
	durabilityLevel DurabilityLevel, durabilityLevelTimeout time.Duration,
	preserveExpiry bool,
	buf []byte,
) (Magic, []byte, error) {
	var err error

	if onBehalfOf != "" {
		buf, err = AppendExtFrame(ExtFrameCodeReqOnBehalfOf, []byte(onBehalfOf), buf)
		if err != nil {
			return 0, nil, err
		}
	}

	if durabilityLevel > 0 {
		if !o.DurabilityEnabled {
			return 0, nil, protocolError{"cannot use synchronous durability when its not enabled"}
		}

		if durabilityLevelTimeout == 0 {
			buf, err = AppendExtFrame(ExtFrameCodeReqDurability, []byte{byte(durabilityLevel)}, buf)
			if err != nil {
				return 0, nil, err
			}
		} else {
			durabilityTimeoutMillis := durabilityLevelTimeout / time.Millisecond
			if durabilityTimeoutMillis > 65535 {
				durabilityTimeoutMillis = 65535
			}
			duraBuf := make([]byte, 3)
			duraBuf[0] = byte(durabilityLevel)
			duraBuf[1] = uint8(durabilityTimeoutMillis >> 8)
			duraBuf[2] = byte(durabilityTimeoutMillis)
			buf, err = AppendExtFrame(ExtFrameCodeReqDurability, duraBuf, buf)
		}
	} else if durabilityLevelTimeout > 0 {
		return 0, nil, protocolError{"cannot encode durability timeout without durability level"}
	}

	if preserveExpiry {
		if !o.PreserveExpiryEnabled {
			return 0, nil, protocolError{"cannot use preserve expiry when its not enabled"}
		}

		buf, err = AppendExtFrame(ExtFrameCodeReqPreserveTTL, nil, buf)
		if err != nil {
			return 0, nil, err
		}
	}

	if len(buf) > 0 {
		if !o.ExtFramesEnabled {
			return 0, nil, protocolError{"cannot use framing extras when its not enabled"}
		}

		return MagicReqExt, buf, nil
	}

	return MagicReq, nil, nil
}

func (o OpsCrud) decodeCommonStatus(status Status) error {
	switch status {
	case StatusCollectionUnknown:
		return ErrUnknownCollectionID
	case StatusAccessError:
		return ErrAccessError
	default:
		return nil
	}
}
func (o OpsCrud) decodeCommonError(resp *Packet, dispatchedTo string, dispatchedFrom string) error {
	err := OpsCrud{}.decodeCommonStatus(resp.Status)
	if err != nil {
		return err
	}

	return OpsCore{}.decodeError(resp, dispatchedTo, dispatchedFrom)
}

type GetRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16

	OnBehalfOf string
}

type GetResponse struct {
	Cas      uint64
	Flags    uint32
	Value    []byte
	Datatype uint8
}

func (o OpsCrud) Get(d Dispatcher, req *GetRequest, cb func(*GetResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGet,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 4 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		flags := binary.BigEndian.Uint32(resp.Extras[0:])

		cb(&GetResponse{
			Cas:      resp.Cas,
			Flags:    flags,
			Value:    resp.Value,
			Datatype: resp.Datatype,
		}, nil)
		return false
	})
}

type GetAndTouchRequest struct {
	CollectionID uint32
	Expiry       uint32
	Key          []byte
	VbucketID    uint16
	OnBehalfOf   string
}

type GetAndTouchResponse struct {
	Cas      uint64
	Flags    uint32
	Value    []byte
	Datatype uint8
}

func (o OpsCrud) GetAndTouch(d Dispatcher, req *GetAndTouchRequest, cb func(*GetAndTouchResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(extraBuf[0:], req.Expiry)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGAT,
		Key:           reqKey,
		Extras:        extraBuf,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusKeyExists {
			cb(nil, ErrDocLocked)
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 4 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		flags := binary.BigEndian.Uint32(resp.Extras[0:])

		cb(&GetAndTouchResponse{
			Cas:      resp.Cas,
			Flags:    flags,
			Value:    resp.Value,
			Datatype: resp.Datatype,
		}, nil)
		return false
	})
}

type GetReplicaRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16
	OnBehalfOf   string
}

type GetReplicaResponse struct {
	Cas      uint64
	Flags    uint32
	Value    []byte
	Datatype uint8
}

func (o OpsCrud) GetReplica(d Dispatcher, req *GetReplicaRequest, cb func(*GetReplicaResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGetReplica,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusKeyExists {
			cb(nil, ErrDocLocked)
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 4 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		flags := binary.BigEndian.Uint32(resp.Extras[0:])

		cb(&GetReplicaResponse{
			Cas:      resp.Cas,
			Flags:    flags,
			Value:    resp.Value,
			Datatype: resp.Datatype,
		}, nil)
		return false
	})
}

type GetAndLockRequest struct {
	CollectionID uint32
	LockTime     uint32
	Key          []byte
	VbucketID    uint16

	OnBehalfOf string
}

type GetAndLockResponse struct {
	Cas      uint64
	Flags    uint32
	Value    []byte
	Datatype uint8
}

func (o OpsCrud) GetAndLock(d Dispatcher, req *GetAndLockRequest, cb func(*GetAndLockResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(extraBuf[0:], req.LockTime)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGetLocked,
		Key:           reqKey,
		Extras:        extraBuf,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusKeyExists {
			cb(nil, ErrDocLocked)
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 4 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		flags := binary.BigEndian.Uint32(resp.Extras[0:])

		cb(&GetAndLockResponse{
			Cas:      resp.Cas,
			Flags:    flags,
			Value:    resp.Value,
			Datatype: resp.Datatype,
		}, nil)
		return false
	})
}

type GetRandomRequest struct {
	CollectionID uint32

	OnBehalfOf string
}

type GetRandomResponse struct {
	Key      []byte
	Cas      uint64
	Flags    uint32
	Value    []byte
	Datatype uint8
}

func (o OpsCrud) GetRandom(d Dispatcher, req *GetRandomRequest, cb func(*GetRandomResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	var extrasBuf []byte
	if o.CollectionsEnabled {
		extrasBuf = make([]byte, 4)
		binary.BigEndian.PutUint32(extrasBuf, req.CollectionID)
	} else {
		if req.CollectionID != 0 {
			return nil, ErrCollectionsNotEnabled
		}

		// extrasBuf = nil
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGetRandom,
		Extras:        extrasBuf,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 4 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		flags := binary.BigEndian.Uint32(resp.Extras[0:])

		cb(&GetRandomResponse{
			Key:      resp.Key,
			Cas:      resp.Cas,
			Flags:    flags,
			Value:    resp.Value,
			Datatype: resp.Datatype,
		}, nil)
		return false
	})
}

type SetRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Flags                  uint32
	Value                  []byte
	Datatype               uint8
	Expiry                 uint32
	PreserveExpiry         bool
	OnBehalfOf             string
	Cas                    uint64
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type SetResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Set(d Dispatcher, req *SetRequest, cb func(*SetResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		req.PreserveExpiry,
		nil)

	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 8)
	binary.BigEndian.PutUint32(extraBuf[0:], req.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], req.Expiry)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeSet,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      req.Datatype,
		Extras:        extraBuf,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
		Cas:           req.Cas,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists {
			cb(nil, ErrCasMismatch)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&SetResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type UnlockRequest struct {
	CollectionID uint32
	Cas          uint64
	Key          []byte
	VbucketID    uint16

	OnBehalfOf string
}

type UnlockResponse struct {
	MutationToken MutationToken
}

func (o OpsCrud) Unlock(d Dispatcher, req *UnlockRequest, cb func(*UnlockResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeUnlockKey,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Cas:           req.Cas,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusTmpFail {
			cb(nil, ErrDocLocked)
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&UnlockResponse{
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type TouchRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16
	Expiry       uint32
	OnBehalfOf   string
}

type TouchResponse struct {
	Cas uint64
}

func (o OpsCrud) Touch(d Dispatcher, req *TouchRequest, cb func(*TouchResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(extraBuf[0:], req.Expiry)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeTouch,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Extras:        extraBuf,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusKeyExists {
			cb(nil, ErrDocLocked)
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&TouchResponse{
			Cas: resp.Cas,
		}, nil)
		return false
	})
}

type DeleteRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	OnBehalfOf             string
	Cas                    uint64
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type DeleteResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Delete(d Dispatcher, req *DeleteRequest, cb func(*DeleteResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeDelete,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
		Cas:           req.Cas,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists {
			cb(nil, ErrCasMismatch)
			return false
		} else if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&DeleteResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type AddRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Flags                  uint32
	Value                  []byte
	Datatype               uint8
	Expiry                 uint32
	OnBehalfOf             string
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type AddResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Add(d Dispatcher, req *AddRequest, cb func(*AddResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 8)
	binary.BigEndian.PutUint32(extraBuf[0:], req.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], req.Expiry)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeAdd,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      req.Datatype,
		Extras:        extraBuf,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists {
			cb(nil, ErrDocExists)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&AddResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type ReplaceRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Flags                  uint32
	Value                  []byte
	Datatype               uint8
	Expiry                 uint32
	PreserveExpiry         bool
	OnBehalfOf             string
	Cas                    uint64
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type ReplaceResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Replace(d Dispatcher, req *ReplaceRequest, cb func(*ReplaceResponse, error)) (PendingOp, error) {
	if req.Expiry != 0 && req.PreserveExpiry {
		cb(nil, protocolError{"cannot specify expiry and preserve expiry"})
	}

	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		req.PreserveExpiry,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 8)
	binary.BigEndian.PutUint32(extraBuf[0:], req.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], req.Expiry)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeReplace,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      req.Datatype,
		Extras:        extraBuf,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
		Cas:           req.Cas,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists {
			cb(nil, ErrCasMismatch)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&ReplaceResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type AppendRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Value                  []byte
	OnBehalfOf             string
	Datatype               uint8
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type AppendResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Append(d Dispatcher, req *AppendRequest, cb func(*AppendResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeAppend,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
		Datatype:      req.Datatype,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusNotStored {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&AppendResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type PrependRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Value                  []byte
	OnBehalfOf             string
	Datatype               uint8
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type PrependResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) Prepend(d Dispatcher, req *PrependRequest, cb func(*PrependResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodePrepend,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
		Datatype:      req.Datatype,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusNotStored {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&PrependResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type IncrementRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	OnBehalfOf             string
	Initial                uint64
	Delta                  uint64
	Expiry                 uint32
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type IncrementResponse struct {
	Cas           uint64
	MutationToken MutationToken
	Value         uint64
}

func (o OpsCrud) Increment(d Dispatcher, req *IncrementRequest, cb func(*IncrementResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 20)
	binary.BigEndian.PutUint64(extraBuf[0:], req.Delta)
	if req.Initial != uint64(0xFFFFFFFFFFFFFFFF) {
		binary.BigEndian.PutUint64(extraBuf[8:], req.Initial)
		binary.BigEndian.PutUint32(extraBuf[16:], req.Expiry)
	} else {
		binary.BigEndian.PutUint64(extraBuf[8:], 0x0000000000000000)
		binary.BigEndian.PutUint32(extraBuf[16:], 0xFFFFFFFF)
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeIncrement,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      0,
		Extras:        extraBuf,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Value) != 8 {
			cb(nil, protocolError{"bad value length"})
			return false
		}
		intVal := binary.BigEndian.Uint64(resp.Value)

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&IncrementResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
			Value:         intVal,
		}, nil)
		return false
	})
}

type DecrementRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	OnBehalfOf             string
	Initial                uint64
	Delta                  uint64
	Expiry                 uint32
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type DecrementResponse struct {
	Cas           uint64
	MutationToken MutationToken
	Value         uint64
}

func (o OpsCrud) Decrement(d Dispatcher, req *DecrementRequest, cb func(*DecrementResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		false,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 20)
	binary.BigEndian.PutUint64(extraBuf[0:], req.Delta)
	if req.Initial != uint64(0xFFFFFFFFFFFFFFFF) {
		binary.BigEndian.PutUint64(extraBuf[8:], req.Initial)
		binary.BigEndian.PutUint32(extraBuf[16:], req.Expiry)
	} else {
		binary.BigEndian.PutUint64(extraBuf[8:], 0x0000000000000000)
		binary.BigEndian.PutUint32(extraBuf[16:], 0xFFFFFFFF)
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeDecrement,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      0,
		Extras:        extraBuf,
		FramingExtras: extFramesBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Value) != 8 {
			cb(nil, protocolError{"bad value length"})
			return false
		}
		intVal := binary.BigEndian.Uint64(resp.Value)

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&DecrementResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
			Value:         intVal,
		}, nil)
		return false
	})
}

type GetMetaRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16

	OnBehalfOf string
}

type GetMetaResponse struct {
	Value    []byte
	Flags    uint32
	Cas      uint64
	Expiry   uint32
	SeqNo    uint64
	Datatype uint8
	Deleted  uint32
}

func (o OpsCrud) GetMeta(d Dispatcher, req *GetMetaRequest, cb func(*GetMetaResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	// This appears to be necessary to get the server to include the datatype in the response
	// extras.
	extraBuf := make([]byte, 1)
	extraBuf[0] = 2

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeGetMeta,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
		Extras:        extraBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		if len(resp.Extras) != 21 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		res := &GetMetaResponse{
			Value: resp.Value,
			Cas:   resp.Cas,
		}
		res.Deleted = binary.BigEndian.Uint32(resp.Extras[0:])
		res.Flags = binary.BigEndian.Uint32(resp.Extras[4:])
		res.Expiry = binary.BigEndian.Uint32(resp.Extras[8:])
		res.SeqNo = binary.BigEndian.Uint64(resp.Extras[12:])
		res.Datatype = resp.Extras[20]

		cb(res, nil)
		return false
	})
}

type SetMetaRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16
	Flags        uint32
	Value        []byte
	Datatype     uint8
	Expiry       uint32
	OnBehalfOf   string
	Extra        []byte
	RevNo        uint64
	Cas          uint64
	Options      uint32
}

type SetMetaResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) SetMeta(d Dispatcher, req *SetMetaRequest, cb func(*SetMetaResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 30+len(req.Extra))
	binary.BigEndian.PutUint32(extraBuf[0:], req.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], req.Expiry)
	binary.BigEndian.PutUint64(extraBuf[8:], req.RevNo)
	binary.BigEndian.PutUint64(extraBuf[16:], req.Cas)
	binary.BigEndian.PutUint32(extraBuf[24:], req.Options)
	binary.BigEndian.PutUint16(extraBuf[28:], uint16(len(req.Extra)))
	copy(extraBuf[30:], req.Extra)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeSetMeta,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Datatype:      req.Datatype,
		Extras:        extraBuf,
		Value:         req.Value,
		FramingExtras: extFramesBuf,
		Cas:           0,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists {
			cb(nil, ErrCasMismatch)
			return false
		}

		if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&SetMetaResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type DeleteMetaRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16
	Flags        uint32
	Expiry       uint32
	OnBehalfOf   string
	Cas          uint64
	Extra        []byte
	RevNo        uint64
	Options      uint32
}

type DeleteMetaResponse struct {
	Cas           uint64
	MutationToken MutationToken
}

func (o OpsCrud) DeleteMeta(d Dispatcher, req *DeleteMetaRequest, cb func(*DeleteMetaResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	extraBuf := make([]byte, 30+len(req.Extra))
	binary.BigEndian.PutUint32(extraBuf[0:], req.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], req.Expiry)
	binary.BigEndian.PutUint64(extraBuf[8:], req.RevNo)
	binary.BigEndian.PutUint64(extraBuf[16:], req.Cas)
	binary.BigEndian.PutUint32(extraBuf[24:], req.Options)
	binary.BigEndian.PutUint16(extraBuf[28:], uint16(len(req.Extra)))
	copy(extraBuf[30:], req.Extra)

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeDelMeta,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
		Cas:           req.Cas,
		Extras:        extraBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyExists && req.Cas > 0 {
			cb(nil, ErrCasMismatch)
			return false
		} else if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&DeleteMetaResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
		}, nil)
		return false
	})
}

type LookupInRequest struct {
	CollectionID uint32
	Key          []byte
	VbucketID    uint16
	Flags        SubdocDocFlag
	Ops          []LookupInOp

	OnBehalfOf string
}

type LookupInResponse struct {
	Ops          []SubDocResult
	DocIsDeleted bool
	Cas          uint64
}

func (o OpsCrud) LookupIn(d Dispatcher, req *LookupInRequest, cb func(*LookupInResponse, error)) (PendingOp, error) {
	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(req.OnBehalfOf, 0, 0, false, nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	lenOps := len(req.Ops)
	pathBytesList := make([][]byte, lenOps)
	pathBytesTotal := 0
	for i, op := range req.Ops {
		pathBytes := op.Path
		pathBytesList[i] = pathBytes
		pathBytesTotal += len(pathBytes)
	}

	valueBuf := make([]byte, lenOps*4+pathBytesTotal)

	valueIter := 0
	for i, op := range req.Ops {
		pathBytes := pathBytesList[i]
		pathBytesLen := len(pathBytes)

		valueBuf[valueIter+0] = uint8(op.Op)
		valueBuf[valueIter+1] = uint8(op.Flags)
		binary.BigEndian.PutUint16(valueBuf[valueIter+2:], uint16(pathBytesLen))
		copy(valueBuf[valueIter+4:], pathBytes)
		valueIter += 4 + pathBytesLen
	}

	var extraBuf []byte
	if req.Flags != 0 {
		extraBuf = append(extraBuf, uint8(req.Flags))
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeSubDocMultiLookup,
		Key:           reqKey,
		Extras:        extraBuf,
		VbucketID:     req.VbucketID,
		FramingExtras: extFramesBuf,
		Value:         valueBuf,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status != StatusSuccess && resp.Status != StatusSubDocSuccessDeleted &&
			resp.Status != StatusSubDocMultiPathFailureDeleted && resp.Status != StatusSubDocBadMulti {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		results := make([]SubDocResult, lenOps)
		respIter := 0
		for i := range results {
			if respIter+6 > len(resp.Value) {
				cb(nil, protocolError{"bad value length"})
				return false
			}

			resError := Status(binary.BigEndian.Uint16(resp.Value[respIter+0:]))
			resValueLen := int(binary.BigEndian.Uint32(resp.Value[respIter+2:]))

			if respIter+6+resValueLen > len(resp.Value) {
				cb(nil, protocolError{"bad value length"})
				return false
			}

			if resError != StatusSuccess {
				cause := OpsCrud{}.decodeCommonStatus(resError)
				if cause == nil {
					cause = errors.New("unexpected status: " + resError.String())
				}
				results[i].Err = ServerError{
					Cause:          cause,
					DispatchedTo:   d.RemoteAddr(),
					DispatchedFrom: d.LocalAddr(),
					Opaque:         resp.Opaque,
				}
			}

			results[i].Value = resp.Value[respIter+6 : respIter+6+resValueLen]
			respIter += 6 + resValueLen
		}

		res := &LookupInResponse{
			Ops: results,
			Cas: resp.Cas,
		}
		res.DocIsDeleted = resp.Status == StatusSubDocSuccessDeleted ||
			resp.Status == StatusSubDocMultiPathFailureDeleted

		cb(res, nil)
		return false
	})
}

type MutateInRequest struct {
	CollectionID           uint32
	Key                    []byte
	VbucketID              uint16
	Flags                  SubdocDocFlag
	Ops                    []MutateInOp
	Expiry                 uint32
	PreserveExpiry         bool
	OnBehalfOf             string
	Cas                    uint64
	DurabilityLevel        DurabilityLevel
	DurabilityLevelTimeout time.Duration
}

type MutateInResponse struct {
	Cas           uint64
	MutationToken MutationToken
	Ops           []SubDocResult
}

func (o OpsCrud) MutateIn(d Dispatcher, req *MutateInRequest, cb func(*MutateInResponse, error)) (PendingOp, error) {
	if req.Expiry != 0 && req.PreserveExpiry {
		cb(nil, protocolError{"cannot specify expiry and preserve expiry"})
	}

	reqMagic, extFramesBuf, err := o.encodeReqExtFrames(
		req.OnBehalfOf,
		req.DurabilityLevel, req.DurabilityLevelTimeout,
		req.PreserveExpiry,
		nil)
	if err != nil {
		return nil, err
	}

	reqKey, err := o.encodeCollectionAndKey(req.CollectionID, req.Key, nil)
	if err != nil {
		return nil, err
	}

	lenOps := len(req.Ops)
	pathBytesList := make([][]byte, lenOps)
	pathBytesTotal := 0
	valueBytesTotal := 0
	for i, op := range req.Ops {
		pathBytes := op.Path
		pathBytesList[i] = pathBytes
		pathBytesTotal += len(pathBytes)
		valueBytesTotal += len(op.Value)
	}

	valueBuf := make([]byte, lenOps*8+pathBytesTotal+valueBytesTotal)

	valueIter := 0
	for i, op := range req.Ops {
		// if op.Op == SubDocOpReplaceBodyWithXattr {
		// 	// We can get here before support status is actually known, we'll send the request unless we know for a fact
		// 	// that this is unsupported.
		// 	if crud.featureVerifier.HasBucketCapabilityStatus(BucketCapabilityReplaceBodyWithXattr, BucketCapabilityStatusUnsupported) {
		// 		return nil, errFeatureNotAvailable
		// 	}
		// }

		pathBytes := pathBytesList[i]
		pathBytesLen := len(pathBytes)
		valueBytesLen := len(op.Value)

		valueBuf[valueIter+0] = uint8(op.Op)
		valueBuf[valueIter+1] = uint8(op.Flags)
		binary.BigEndian.PutUint16(valueBuf[valueIter+2:], uint16(pathBytesLen))
		binary.BigEndian.PutUint32(valueBuf[valueIter+4:], uint32(valueBytesLen))
		copy(valueBuf[valueIter+8:], pathBytes)
		copy(valueBuf[valueIter+8+pathBytesLen:], op.Value)
		valueIter += 8 + pathBytesLen + valueBytesLen
	}

	var extraBuf []byte
	if req.Expiry != 0 {
		tmpBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(tmpBuf[0:], req.Expiry)
		extraBuf = append(extraBuf, tmpBuf...)
	}
	if req.Flags != 0 {
		extraBuf = append(extraBuf, uint8(req.Flags))
	}

	return d.Dispatch(&Packet{
		Magic:         reqMagic,
		OpCode:        OpCodeSubDocMultiMutation,
		Key:           reqKey,
		VbucketID:     req.VbucketID,
		Extras:        extraBuf,
		Value:         valueBuf,
		FramingExtras: extFramesBuf,
		Cas:           req.Cas,
	}, func(resp *Packet, err error) bool {
		if err != nil {
			cb(nil, err)
			return false
		}

		if resp.Status == StatusKeyNotFound {
			cb(nil, ErrDocNotFound)
			return false
		} else if resp.Status == StatusNotStored && req.Flags&SubdocDocFlagAddDoc != 0 { // Only doc exists error if flags are add
			cb(nil, ErrDocExists)
			return false
		} else if resp.Status == StatusKeyExists {
			cb(nil, ErrCasMismatch)
			return false
		} else if resp.Status == StatusSubDocBadMulti {
			if len(resp.Value) != 3 {
				cb(nil, protocolError{"bad value length"})
				return false
			}

			// TODO(chvck): improve this error to include all the errors returned.
			resError := Status(binary.BigEndian.Uint16(resp.Value[1:]))

			cause := OpsCrud{}.decodeCommonStatus(resError)
			if cause == nil {
				cause = errors.New("unexpected status: " + resError.String())
			}
			cb(nil, ServerError{
				Cause:          cause,
				DispatchedTo:   d.RemoteAddr(),
				DispatchedFrom: d.LocalAddr(),
				Opaque:         resp.Opaque,
			})
			return false
		} else if resp.Status != StatusSuccess {
			cb(nil, OpsCrud{}.decodeCommonError(resp, d.RemoteAddr(), d.LocalAddr()))
			return false
		}

		results := make([]SubDocResult, lenOps)
		for readPos := uint32(0); readPos < uint32(len(resp.Value)); {
			opIndex := int(resp.Value[readPos+0])
			opStatus := Status(binary.BigEndian.Uint16(resp.Value[readPos+1:]))

			readPos += 3

			if opStatus == StatusSuccess {
				valLength := binary.BigEndian.Uint32(resp.Value[readPos:])
				results[opIndex].Value = resp.Value[readPos+4 : readPos+4+valLength]
				readPos += 4 + valLength
			} else {
				// TODO(chvck): do something?
			}
		}

		mutToken := MutationToken{}
		if len(resp.Extras) == 16 {
			mutToken.VbUuid = binary.BigEndian.Uint64(resp.Extras[0:])
			mutToken.SeqNo = binary.BigEndian.Uint64(resp.Extras[8:])
		} else if len(resp.Extras) != 0 {
			cb(nil, protocolError{"bad extras length"})
			return false
		}

		cb(&MutateInResponse{
			Cas:           resp.Cas,
			MutationToken: mutToken,
			Ops:           results,
		}, nil)
		return false
	})
}
