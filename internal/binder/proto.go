// Package binder is a small pure-Go binder IPC implementation: enough to
// call AIDL services in the Android container and to host services that
// Android calls back.
//
// Only the 64-bit kernel ABI and the AIDL wire format used by Android 11
// and later are implemented.
package binder

// ioctl numbers (64-bit layouts).
const (
	ioctlWriteRead     = 0xC0306201 // BINDER_WRITE_READ, struct binder_write_read
	ioctlSetMaxThreads = 0x40046205 // BINDER_SET_MAX_THREADS
	ioctlVersion       = 0xC0046209 // BINDER_VERSION

	protocolVersion = 8
	mmapSize        = 1024*1024 - 2*4096
)

// Driver commands (BC_*).
const (
	bcTransaction    = 0x40406300
	bcReply          = 0x40406301
	bcFreeBuffer     = 0x40086303
	bcIncrefs        = 0x40046304
	bcAcquire        = 0x40046305
	bcRelease        = 0x40046306
	bcDecrefs        = 0x40046307
	bcIncrefsDone    = 0x40106308
	bcAcquireDone    = 0x40106309
	bcEnterLooper    = 0x0000630c
	bcRequestDeath   = 0x400c630e
	bcDeadBinderDone = 0x40086310
)

// Driver returns (BR_*).
const (
	brError               = 0x80047200
	brOK                  = 0x00007201
	brTransactionSecCtx   = 0x80487202
	brTransaction         = 0x80407202
	brReply               = 0x80407203
	brDeadReply           = 0x00007205
	brTransactionComplete = 0x00007206
	brIncrefs             = 0x80107207
	brAcquire             = 0x80107208
	brRelease             = 0x80107209
	brDecrefs             = 0x8010720a
	brNoop                = 0x0000720c
	brSpawnLooper         = 0x0000720d
	brDeadBinder          = 0x8008720f
	brClearDeathDone      = 0x80087210
	brFailedReply         = 0x00007211
	brFrozenReply         = 0x00007212
)

// Transaction flags.
const (
	FlagOneWay     = 0x01
	flagStatusCode = 0x08
	flagAcceptFDs  = 0x10
)

// Special transaction codes every binder object understands.
const (
	FirstCall            = 0x00000001
	pingTransaction      = 0x5f504e47 // '_PNG'
	interfaceTransaction = 0x5f4e5446 // '_NTF'
)

// flat_binder_object types and flags.
const (
	typeBinder = 0x73622a85 // 's','b','*',0x85
	typeHandle = 0x73682a85 // 's','h','*',0x85

	flagsLocalObject = 0x7f | 0x100 // priority mask | FLAT_BINDER_FLAG_ACCEPTS_FDS

	flatObjectSize = 24
	stabilitySize  = 4 // aidl3: int32 stability after each binder object

	StabilitySystem = 0x0c
)

// RPC header fields (aidl3).
const (
	strictModePenaltyGather = 0x40 << 16
	unsetWorkSource         = -1
	sysHeader               = 0x53595354 // 'SYST'
)

// Status codes (negative errno, as in libbinder).
const (
	statusUnknownTransaction = -74 // -EBADMSG
)

// AIDL exception codes found in reply headers.
const (
	exHasNotedAppOpsReplyHeader = -127
	exHasReplyHeader            = -128
)

// payloadSize extracts the argument size encoded in a BC/BR command.
func payloadSize(cmd uint32) int { return int((cmd >> 16) & 0x3fff) }
