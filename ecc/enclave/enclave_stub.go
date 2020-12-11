/*
   Copyright 2019 Intel Corporation
   Copyright IBM Corp. All Rights Reserved.

   SPDX-License-Identifier: Apache-2.0
*/

/*
TODO:
- add everywhere explicit (& consistent) return errors to ocalls/ecalls
*/

package enclave

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger-labs/fabric-private-chaincode/ecc/crypto"
	fpcpb "github.com/hyperledger-labs/fabric-private-chaincode/internal/protos"
	utils "github.com/hyperledger-labs/fabric-private-chaincode/internal/utils"
	"github.com/hyperledger/fabric-chaincode-go/shim"
	"github.com/hyperledger/fabric-protos-go/msp"
	"github.com/hyperledger/fabric-protos-go/peer"
	"golang.org/x/sync/semaphore"
)

// #cgo CFLAGS: -I${SRCDIR}/ecc-enclave-include -I${SRCDIR}/../../common/sgxcclib
// #cgo LDFLAGS: -L${SRCDIR}/ecc-enclave-lib -lsgxcc
// #include "common-sgxcclib.h"
// #include "sgxcclib.h"
// #include <stdio.h>
// #include <string.h>
//
// /*
//    Below extern definitions should really be done by cgo but without we get following warning:
//       warning: implicit declaration of function ▒▒▒_GoStringPtr_▒▒▒ [-Wimplicit-function-declaration]
// */
// extern const char *_GoStringPtr(_GoString_ s);
// extern size_t _GoStringLen(_GoString_ s);
//
// static inline void _cpy_bytes(uint8_t* target, uint8_t* val, uint32_t size)
// {
//   memcpy(target, val, size);
// }
//
// static inline void _set_int(uint32_t* target, uint32_t val)
// {
//   *target = val;
// }
//
// static inline void _cpy_str(char* target, _GoString_ val, uint32_t max_size)
// {
//   #define MIN(x, y) (((x) < (y)) ? (x) : (y))
//   // Note: have to do MIN as _GoStringPtr() might not be NULL terminated.
//   // Also _GoStringLen returns the length without \0 ...
//   size_t goStrLen = _GoStringLen(val)+1;
//   snprintf(target, MIN(max_size, goStrLen), "%s", _GoStringPtr(val));
// }
//
import "C"

const EPID_SIZE = 8
const SPID_SIZE = 16
const MAX_RESPONSE_SIZE = 1024 * 100 // Let's be really conservative ...
const SIGNATURE_SIZE = 64
const PUB_KEY_SIZE = 64

const TARGET_INFO_SIZE = 512
const CMAC_SIZE = 16
const ENCLAVE_TCS_NUM = 8

// just a container struct used for the callbacks
type Stubs struct {
	shimStub shim.ChaincodeStubInterface
}

// have a global registry
var registry = NewRegistry()

// used to store shims for callbacks
type Registry struct {
	sync.RWMutex
	index    int
	internal map[int]*Stubs
}

func NewRegistry() *Registry {
	return &Registry{
		internal: make(map[int]*Stubs),
	}
}

func (r *Registry) Register(stubs *Stubs) int {
	r.Lock()
	defer r.Unlock()
	r.index++
	r.internal[r.index] = stubs
	return r.index
}

func (r *Registry) Release(i int) {
	r.Lock()
	delete(r.internal, i)
	r.Unlock()
}

func (r *Registry) Get(i int) *Stubs {
	r.RLock()
	stubs, ok := r.internal[i]
	r.RUnlock()
	if !ok {
		panic(fmt.Errorf("No shim for: %d", i))
	}
	return stubs
}

//export get_creator_name
func get_creator_name(msp_id *C.char, max_msp_id_len C.uint32_t, dn *C.char, max_dn_len C.uint32_t, ctx unsafe.Pointer) {
	stubs := registry.Get(*(*int)(ctx))

	// TODO (eventually): replace/simplify below via ext.ClientIdentity,
	// should also make it easier to eventually return more than only
	// msp & dn ..

	serializedID, err := stubs.shimStub.GetCreator()
	if err != nil {
		panic("error while getting creator")
	}
	sId := &msp.SerializedIdentity{}
	err = proto.Unmarshal(serializedID, sId)
	if err != nil {
		panic("Could not deserialize a SerializedIdentity")
	}

	bl, _ := pem.Decode(sId.IdBytes)
	if bl == nil {
		panic("Failed to decode PEM structure")
	}
	cert, err := x509.ParseCertificate(bl.Bytes)
	if err != nil {
		panic("Unable to parse certificate %s")
	}

	var goMspId = sId.Mspid
	C._cpy_str(msp_id, goMspId, max_msp_id_len)

	var goDn = cert.Subject.String()
	C._cpy_str(dn, goDn, max_dn_len)
	// TODO (eventually): return the eror case of the dn buffer being too small
}

//export get_state
func get_state(key *C.char, val *C.uint8_t, max_val_len C.uint32_t, val_len *C.uint32_t, ctx unsafe.Pointer) {
	stubs := registry.Get(*(*int)(ctx))

	// check if composite key
	key_str := C.GoString(key)
	if utils.IsFPCCompositeKey(key_str) {
		comp := utils.SplitFPCCompositeKey(key_str)
		key_str, _ = stubs.shimStub.CreateCompositeKey(comp[0], comp[1:])
	}

	data, err := stubs.shimStub.GetState(key_str)
	if err != nil {
		panic("error while getting state")
	}
	if C.uint32_t(len(data)) > max_val_len {
		C._set_int(val_len, C.uint32_t(0))
		// NOTE: there is currently no way to explicitly return an error
		// to distinguish from absence of key.  However, iff key exist
		// and we return an error, this should trigger an integrity
		// error, so the shim implicitly notice the difference.
		return
	}
	C._cpy_bytes(val, (*C.uint8_t)(C.CBytes(data)), C.uint32_t(len(data)))
	C._set_int(val_len, C.uint32_t(len(data)))
}

//export put_state
func put_state(key *C.char, val unsafe.Pointer, val_len C.int, ctx unsafe.Pointer) {
	stubs := registry.Get(*(*int)(ctx))

	// check if composite key
	key_str := C.GoString(key)
	if utils.IsFPCCompositeKey(key_str) {
		comp := utils.SplitFPCCompositeKey(key_str)
		key_str, _ = stubs.shimStub.CreateCompositeKey(comp[0], comp[1:])
	}

	if stubs.shimStub.PutState(key_str, C.GoBytes(val, val_len)) != nil {
		panic("error while putting state")
	}
}

//export get_state_by_partial_composite_key
func get_state_by_partial_composite_key(comp_key *C.char, values *C.uint8_t, max_values_len C.uint32_t, values_len *C.uint32_t, ctx unsafe.Pointer) {
	stubs := registry.Get(*(*int)(ctx))

	// split and get a proper composite key
	comp := utils.SplitFPCCompositeKey(C.GoString(comp_key))
	iter, err := stubs.shimStub.GetStateByPartialCompositeKey(comp[0], comp[1:])
	if err != nil {
		panic("error while range query")
	}
	defer iter.Close()

	var buf bytes.Buffer
	buf.WriteString("[")
	for iter.HasNext() {
		item, err := iter.Next()
		if err != nil {
			panic("Error " + err.Error())
		}
		buf.WriteString("{\"key\":\"")
		buf.WriteString(utils.TransformToFPCKey(item.Key))
		buf.WriteString("\",\"value\":\"")
		buf.Write(item.Value)
		if iter.HasNext() {
			buf.WriteString("\"},")
		} else {
			buf.WriteString("\"}")
		}
	}
	buf.WriteString("]")
	data := buf.Bytes()

	if C.uint32_t(len(data)) > max_values_len {
		C._set_int(values_len, C.uint32_t(0))
		// NOTE: there is currently no way to explicitly return an error
		// to distinguish from absence of key.  However, iff key exist
		// and we return an error, this should trigger an integrity
		// error, so the shim implicitly notice the difference.
		return
	}
	C._cpy_bytes(values, (*C.uint8_t)(C.CBytes(data)), C.uint32_t(len(data)))
	C._set_int(values_len, C.uint32_t(len(data)))
}

// Stub interface
type Stub interface {
	// Return quote and enclave PK in DER-encoded PKIX format
	GetRemoteAttestationReport(spid []byte, sig_rl []byte, sig_rl_size uint) ([]byte, []byte, error)
	// Return report and enclave PK in DER-encoded PKIX format
	GetLocalAttestationReport(targetInfo []byte) ([]byte, []byte, error)
	// Invoke chaincode
	Invoke(shimStub shim.ChaincodeStubInterface) ([]byte, error)
	// Creates an enclave from a given enclave lib file
	Create(enclaveLibFile string, ccParametersBytes []byte, attestationParametersBytes []byte, hostParametersBytes []byte) ([]byte, error)
	// Gets Enclave Target Information
	GetTargetInfo() ([]byte, error)
	// Bind to tlcc
	Bind(report, pk []byte) error
	// Destroys enclave
	Destroy() error
	// Returns expected MRENCLAVE
	MrEnclave() (string, error)
}

// StubImpl implements the interface
type StubImpl struct {
	eid C.enclave_id_t
	sem *semaphore.Weighted
}

// NewEnclave starts a new enclave
func NewEnclave() Stub {
	return &StubImpl{sem: semaphore.NewWeighted(ENCLAVE_TCS_NUM)}
}

// GetRemoteAttestationReport - calls the enclave for attestation, takes SPID as input
// and returns a quote and enclaves public key
func (e *StubImpl) GetRemoteAttestationReport(spid []byte, sig_rl []byte, sig_rl_size uint) ([]byte, []byte, error) {
	//sig_rl
	var sig_rlPtr unsafe.Pointer
	if sig_rl == nil {
		sig_rlPtr = unsafe.Pointer(sig_rlPtr)
	} else {
		sig_rlPtr = C.CBytes(sig_rl)
		defer C.free(sig_rlPtr)
	}

	// quote size
	quoteSize := C.uint32_t(0)
	ret := C.sgxcc_get_quote_size((*C.uint8_t)(sig_rlPtr), C.uint(sig_rl_size), (*C.uint32_t)(unsafe.Pointer(&quoteSize)))
	if ret != 0 {
		return nil, nil, fmt.Errorf("C.sgxcc_get_quote_size failed. Reason: %d", int(ret))
	}

	// pubkey
	pubkeyPtr := C.malloc(PUB_KEY_SIZE)
	defer C.free(pubkeyPtr)

	// spid
	spidPtr := C.CBytes(spid)
	defer C.free(spidPtr)

	// prepare quote space
	quotePtr := C.malloc(C.ulong(quoteSize))
	defer C.free(quotePtr)

	// call enclave
	e.sem.Acquire(context.Background(), 1)
	ret = C.sgxcc_get_remote_attestation_report(e.eid, (*C.quote_t)(quotePtr), C.uint32_t(quoteSize),
		(*C.ec256_public_t)(pubkeyPtr), (*C.spid_t)(spidPtr), (*C.uint8_t)(sig_rlPtr), C.uint32_t(sig_rl_size))
	e.sem.Release(1)
	if ret != 0 {
		return nil, nil, fmt.Errorf("C.sgxcc_get_remote_attestation_report failed. Reason: %d", int(ret))
	}

	// convert sgx format to DER-encoded PKIX format
	pk, err := crypto.MarshalEnclavePk(C.GoBytes(pubkeyPtr, C.int(PUB_KEY_SIZE)))
	if err != nil {
		return nil, nil, err
	}

	return C.GoBytes(quotePtr, C.int(quoteSize)), pk, nil
}

// GetLocalAttestationReport - calls the enclave for attestation, takes SPID as input
// and returns a quote and enclaves public key
func (e *StubImpl) GetLocalAttestationReport(spid []byte) ([]byte, []byte, error) {
	// NOT IMPLEMENTED YET
	return nil, nil, nil
}

// invoke calls the enclave for transaction processing, takes arguments
// and the current chaincode state as input and returns a new chaincode state
func (e *StubImpl) Invoke(shimStub shim.ChaincodeStubInterface) ([]byte, error) {
	var err error

	if shimStub == nil {
		return nil, errors.New("Need shim")
	}

	index := registry.Register(&Stubs{shimStub})
	defer registry.Release(index)
	ctx := unsafe.Pointer(&index)

	// response
	cresmProtoBytesLenOut := C.uint32_t(0) // We pass maximal length separatedly; set to zero so we can detect valid responses
	cresmProtoBytesPtr := C.malloc(MAX_RESPONSE_SIZE)
	defer C.free(cresmProtoBytesPtr)

	// get signed proposal
	signedProposal, err := shimStub.GetSignedProposal()
	if err != nil {
		return nil, fmt.Errorf("cannot get signed proposal")
	}
	signedProposalBytes, err := proto.Marshal(signedProposal)
	if err != nil {
		return nil, fmt.Errorf("cannot get signed proposal bytes")
	}
	signedProposalPtr := C.CBytes(signedProposalBytes)
	defer C.free(unsafe.Pointer(signedProposalPtr))

	//ASSUME HERE input is not the protobuf, so let's buildit (rmeove block later)
	argss := shimStub.GetStringArgs()
	argsByteArray := make([][]byte, len(argss))
	for i, v := range argss {
		argsByteArray[i] = []byte(v)
		logger.Debugf("arg %d: %s", i, argsByteArray[i])
	}
	cleartextChaincodeRequestMessageProto := &fpcpb.CleartextChaincodeRequest{
		Input: &peer.ChaincodeInput{Args: argsByteArray},
	}
	cleartextChaincodeRequestMessageProtoBytes, err := proto.Marshal(cleartextChaincodeRequestMessageProto)
	if err != nil {
		return nil, fmt.Errorf("marshal error")
	}
	crmProto := &fpcpb.ChaincodeRequestMessage{
		// TODO: eventually this should be an encrypted CleartextRequestMessage
		EncryptedRequest: cleartextChaincodeRequestMessageProtoBytes,
	}
	crmProtoBytes, err := proto.Marshal(crmProto)
	if err != nil {
		return nil, fmt.Errorf("marshal error")
	}
	crmProtoBytesPtr := C.CBytes(crmProtoBytes)
	defer C.free(unsafe.Pointer(crmProtoBytesPtr))
	//REMOVE BLOCK ABOVE once protobuf supported e2e

	e.sem.Acquire(context.Background(), 1)
	// invoke enclave
	invoke_ret := C.sgxcc_invoke(e.eid,
		(*C.uint8_t)(signedProposalPtr),
		(C.uint32_t)(len(signedProposalBytes)),
		(*C.uint8_t)(crmProtoBytesPtr),
		(C.uint32_t)(len(crmProtoBytes)),
		(*C.uint8_t)(cresmProtoBytesPtr), (C.uint32_t)(MAX_RESPONSE_SIZE), &cresmProtoBytesLenOut,
		ctx)
	e.sem.Release(1)
	if invoke_ret != 0 {
		return nil, fmt.Errorf("Invoke failed. Reason: %d", int(invoke_ret))
	}
	cresmProtoBytes := C.GoBytes(cresmProtoBytesPtr, C.int(cresmProtoBytesLenOut))

	//ASSUME HERE we get the b64 encoded response protobuf, pull encrypted response out and return it
	cresmProto := &fpcpb.ChaincodeResponseMessage{}
	err = proto.Unmarshal(cresmProtoBytes, cresmProto)
	if err != nil {
		return nil, fmt.Errorf("unmarshal error")
	}

	// TODO: this should be eventually be an (encrypted) fabric Response object rather than the response string ...
	return cresmProto.EncryptedResponse, nil
}

// Create starts a new enclave instance
func (e *StubImpl) Create(enclaveLibFile string, ccParametersBytes []byte, attestationParametersBytes []byte, hostParametersBytes []byte) ([]byte, error) {
	var eid C.enclave_id_t

	// prepare output buffer for credentials
	credentialsBuffer := C.malloc(MAX_RESPONSE_SIZE)
	credentialsBufferMaxSize := C.uint32_t(MAX_RESPONSE_SIZE)
	defer C.free(credentialsBuffer)
	credentialsSize := C.uint32_t(0)

	e.sem.Acquire(context.Background(), 1)

	if ret := C.sgxcc_create_enclave(
		&eid,
		C.CString(enclaveLibFile),
		(*C.uint8_t)(C.CBytes(attestationParametersBytes)),
		C.uint32_t(len(attestationParametersBytes)),
		(*C.uint8_t)(C.CBytes(ccParametersBytes)),
		C.uint32_t(len(ccParametersBytes)),
		(*C.uint8_t)(C.CBytes(hostParametersBytes)),
		C.uint32_t(len(hostParametersBytes)),
		(*C.uint8_t)(credentialsBuffer),
		credentialsBufferMaxSize,
		&credentialsSize); ret != 0 {
		return nil, fmt.Errorf("Can not create enclave (lib %s): Reason: %d", enclaveLibFile, ret)
	}
	e.eid = eid
	e.sem.Release(1)
	logger.Infof("Enclave created with %d", e.eid)

	// return credential bytes from sgx call
	return C.GoBytes(credentialsBuffer, C.int(credentialsSize)), nil
}

func (e *StubImpl) GetTargetInfo() ([]byte, error) {
	targetInfoPtr := C.malloc(TARGET_INFO_SIZE)
	defer C.free(targetInfoPtr)

	e.sem.Acquire(context.Background(), 1)
	ret := C.sgxcc_get_target_info(e.eid, (*C.target_info_t)(targetInfoPtr))
	if ret != 0 {
		return nil, fmt.Errorf("C.sgxcc_get_target_info failed. Reason: %d", int(ret))
	}
	e.sem.Release(1)

	return C.GoBytes(targetInfoPtr, TARGET_INFO_SIZE), nil
}

func (e *StubImpl) Bind(report, pk []byte) error {
	// Attention!!!!
	// here we set the report and pk pointer to NULL if not provided
	if report == nil || pk == nil {
		logger.Infof("No report pk provided! Call bind with NULL")
		e.sem.Acquire(context.Background(), 1)
		C.sgxcc_bind(e.eid, (*C.report_t)(nil), (*C.ec256_public_t)(nil))
		e.sem.Release(1)
		return nil
	}

	reportPtr := C.CBytes(report)
	defer C.free(reportPtr)

	// TODO transform pk to sgx
	transPk, err := crypto.UnmarshalEnclavePk(pk)
	if err != nil {
		return err
	}
	pkPtr := C.CBytes(transPk)
	defer C.free(pkPtr)

	e.sem.Acquire(context.Background(), 1)
	C.sgxcc_bind(e.eid, (*C.report_t)(reportPtr), (*C.ec256_public_t)(pkPtr))
	e.sem.Release(1)
	return nil
}

// Destroy kills the current enclave instance
func (e *StubImpl) Destroy() error {
	ret := C.sgxcc_destroy_enclave(e.eid)
	if ret != 0 {
		return fmt.Errorf("C.sgxcc_destroy_enclave failed. Reason: %d", int(ret))
	}
	return nil
}

func (e *StubImpl) MrEnclave() (string, error) {
	f, err := os.Open("mrenclave")
	if err != nil {
		return "", fmt.Errorf("error reading MrEnclave from file: Reason %s", err.Error())
	}
	defer f.Close()

	// read just a single line
	scanner := bufio.NewScanner(f)
	scanner.Scan()
	mrenclave := strings.TrimSuffix(scanner.Text(), "\n")

	// check size
	if len(mrenclave) != 64 {
		return "", fmt.Errorf("error reading MrEnclave from file: Reason wrong size. Expected 64 but read %d: %s", len(mrenclave), mrenclave)
	}

	// check that MrEnclave comes as hex string
	_, err = hex.DecodeString(mrenclave)
	if err != nil {
		return "", fmt.Errorf("error reading MrEnclave from file: Reason %s", err.Error())
	}

	return mrenclave, nil
}
