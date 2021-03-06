/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package endorser

import (
	"fmt"

	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/crypto"
	"github.com/hyperledger/fabric/core/aclmgmt"
	"github.com/hyperledger/fabric/core/aclmgmt/resources"
	"github.com/hyperledger/fabric/core/chaincode"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/handlers/decoration"
	. "github.com/hyperledger/fabric/core/handlers/endorsement/api/identities"
	"github.com/hyperledger/fabric/core/handlers/library"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/peer"
	"github.com/hyperledger/fabric/core/scc"
	"github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	"github.com/pkg/errors"
)

// SupportImpl provides an implementation of the endorser.Support interface
// issuing calls to various static methods of the peer
type SupportImpl struct {
	*PluginEndorser
	crypto.SignerSupport
	Peer             peer.Operations
	PeerSupport      peer.Support
	ChaincodeSupport []*chaincode.ChaincodeSupport
	SysCCProvider    *scc.Provider
	ACLProvider      aclmgmt.ACLProvider
}

func (s *SupportImpl) NewQueryCreator(channel string) (QueryCreator, error) {
	lgr := s.Peer.GetLedger(channel)
	if lgr == nil {
		return nil, errors.Errorf("channel %s doesn't exist", channel)
	}
	return lgr, nil
}

func (s *SupportImpl) SigningIdentityForRequest(*pb.SignedProposal) (SigningIdentity, error) {
	return s.SignerSupport, nil
}

// IsSysCCAndNotInvokableExternal returns true if the supplied chaincode is
// ia system chaincode and it NOT invokable
func (s *SupportImpl) IsSysCCAndNotInvokableExternal(name string) bool {
	return s.SysCCProvider.IsSysCCAndNotInvokableExternal(name)
}

// GetTxSimulator returns the transaction simulator for the specified ledger
// a client may obtain more than one such simulator; they are made unique
// by way of the supplied txid
func (s *SupportImpl) GetTxSimulator(ledgername string, txid string) (ledger.TxSimulator, error) {
	lgr := s.Peer.GetLedger(ledgername)
	if lgr == nil {
		return nil, errors.Errorf("Channel does not exist: %s", ledgername)
	}
	return lgr.NewTxSimulator(txid)
}

// GetHistoryQueryExecutor gives handle to a history query executor for the
// specified ledger
func (s *SupportImpl) GetHistoryQueryExecutor(ledgername string) (ledger.HistoryQueryExecutor, error) {
	lgr := s.Peer.GetLedger(ledgername)
	if lgr == nil {
		return nil, errors.Errorf("Channel does not exist: %s", ledgername)
	}
	return lgr.NewHistoryQueryExecutor()
}

// GetTransactionByID retrieves a transaction by id
func (s *SupportImpl) GetTransactionByID(chid, txID string) (*pb.ProcessedTransaction, error) {
	// 根据chid，获取对应通道的账本管理器
	lgr := s.Peer.GetLedger(chid)
	if lgr == nil {
		return nil, errors.Errorf("failed to look up the ledger for Channel %s", chid)
	}
	// 在区块文件中查找交易txID，并获取交易状态码（是否有效）
	tx, err := lgr.GetTransactionByID(txID)
	if err != nil {
		return nil, errors.WithMessage(err, "GetTransactionByID failed")
	}
	return tx, nil
}

// GetLedgerHeight returns ledger height for given channelID
func (s *SupportImpl) GetLedgerHeight(channelID string) (uint64, error) {
	lgr := s.Peer.GetLedger(channelID)
	if lgr == nil {
		return 0, errors.Errorf("failed to look up the ledger for Channel %s", channelID)
	}

	info, err := lgr.GetBlockchainInfo()
	if err != nil {
		return 0, errors.Wrap(err, fmt.Sprintf("failed to obtain information for Channel %s", channelID))
	}

	return info.Height, nil
}

// IsSysCC returns true if the name matches a system chaincode's
// system chaincode names are system, chain wide
func (s *SupportImpl) IsSysCC(name string) bool {
	return s.SysCCProvider.IsSysCC(name)
}

// GetChaincode returns the CCPackage from the fs
func (s *SupportImpl) GetChaincodeDeploymentSpecFS(cds *pb.ChaincodeDeploymentSpec) (*pb.ChaincodeDeploymentSpec, error) {
	ccpack, err := ccprovider.GetChaincodeFromFS(cds.ChaincodeSpec.ChaincodeId.Name, cds.ChaincodeSpec.ChaincodeId.Version)
	if err != nil {
		return nil, errors.Wrapf(err, "could not get chaincode from fs")
	}

	return ccpack.GetDepSpec(), nil
}

// ExecuteInit a deployment proposal and return the chaincode response
func (s *SupportImpl) ExecuteLegacyInit(txParams *ccprovider.TransactionParams, cid, name, version, txid string, signedProp *pb.SignedProposal, prop *pb.Proposal, cds *pb.ChaincodeDeploymentSpec) (*pb.Response, *pb.ChaincodeEvent, error) {
	cccid := &ccprovider.CCContext{
		Name:    name,
		Version: version,
	}
	support := s.ChaincodeSupport[0]
	return support.ExecuteLegacyInit(txParams, cccid, cds)
}

// Execute a proposal and return the chaincode response
//　执行提案并返回链码响应
func (s *SupportImpl) Execute(txParams *ccprovider.TransactionParams, cid, name, version, txid string, signedProp *pb.SignedProposals, props []*pb.Proposal, inputs []*pb.ChaincodeInput) ([]*pb.Response, []*pb.ChaincodeEvent, error) {
	var resps = *new([]*pb.Response)
	var ccEvents = *new([]*pb.ChaincodeEvent)
	var err error

	// decorate the chaincode input
	// 单例装饰器
	decorators := library.InitRegistry(library.Config{}).Lookup(library.Decoration).([]decoration.Decorator)

	for i, _ := range inputs {
		input := inputs[i]
		input.Decorations = make(map[string][]byte) // 目前还没有添加任何修饰
		input = decoration.Apply(props[i], input, decorators...)
		txParams.ProposalDecorations = input.Decorations
		inputs[i] = input // 将装饰后的input重新放入inputs
	}

	cRes := make(chan *pb.Response, 2)
	cCCEvt := make(chan *pb.ChaincodeEvent, 2)
	cErr := make(chan error, 2)

	// 并行执行交易
	for i, _ := range inputs {
		// 此support必须每次循环时创建一个示例，不能在循环时获取for中的value值，因为在for中每次都是为同一个对象赋值
		support := s.ChaincodeSupport[i]
		// 获取配置区块的交易必须使用第一个 ChaincodeSupport，因为创建的其他 ChaincodeSupport 功能不完善。
		if string(inputs[i].Args[0]) == "GetConfigBlock" {
			support = s.ChaincodeSupport[len(s.ChaincodeSupport)-1]
		}

		go func(index int, s *chaincode.ChaincodeSupport) {
			version2 := version
			if (name != "cscc") && (name != "lscc") {
				version2 = version + "-" + s.CCContainerName
			}
			// 创建链码上下文对象
			cccid := &ccprovider.CCContext{
				Name:    name,
				Version: version2,
			}
			response, event, err := s.Execute(txParams, cccid, inputs[index])
			cCCEvt <- event
			cErr <- err
			cRes <- response
		}(i, support)
	}

EXIT:
	for {
		select {
		case err = <-cErr:
			res := <-cRes
			evt := <-cCCEvt
			resps = append(resps, res)
			ccEvents = append(ccEvents, evt)
			// 当获取的响应多于链码的输入参数时退出
			if len(resps) >= len(inputs) {
				break EXIT
			}
			if err != nil {
				resps[0] = res
				ccEvents[0] = evt
			}
		}
	}

	//for i := 1; i < cap(cRes); i++ {
	//	resps = append(resps, <- cRes)
	//	ccEvents = append(ccEvents, <- cCCEvt)
	//}
	return resps, ccEvents, err
}

// GetChaincodeDefinition returns ccprovider.ChaincodeDefinition for the chaincode with the supplied name
func (s *SupportImpl) GetChaincodeDefinition(chaincodeName string, txsim ledger.QueryExecutor) (ccprovider.ChaincodeDefinition, error) {
	support := s.ChaincodeSupport[0]
	return support.Lifecycle.ChaincodeDefinition(chaincodeName, txsim)
}

// CheckACL checks the ACL for the resource for the Channel using the
// SignedProposal from which an id can be extracted for testing against a policy
func (s *SupportImpl) CheckACL(signedProp *pb.SignedProposal, chdr *common.ChannelHeader, shdr *common.SignatureHeader, hdrext *pb.ChaincodeHeaderExtension) error {
	return s.ACLProvider.CheckACL(resources.Peer_Propose, chdr.ChannelId, signedProp)
}

// IsJavaCC returns true if the CDS package bytes describe a chaincode
// that requires the java runtime environment to execute
func (s *SupportImpl) IsJavaCC(buf []byte) (bool, error) {
	//the inner dep spec will contain the type
	ccpack, err := ccprovider.GetCCPackage(buf)
	if err != nil {
		return false, err
	}
	cds := ccpack.GetDepSpec()
	return (cds.ChaincodeSpec.Type == pb.ChaincodeSpec_JAVA), nil
}

// CheckInstantiationPolicy returns an error if the instantiation in the supplied
// ChaincodeDefinition differs from the instantiation policy stored on the ledger
func (s *SupportImpl) CheckInstantiationPolicy(name, version string, cd ccprovider.ChaincodeDefinition) error {
	return ccprovider.CheckInstantiationPolicy(name, version, cd.(*ccprovider.ChaincodeData))
}

// GetApplicationConfig returns the configtxapplication.SharedConfig for the Channel
// and whether the Application config exists
func (s *SupportImpl) GetApplicationConfig(cid string) (channelconfig.Application, bool) {
	return s.PeerSupport.GetApplicationConfig(cid)
}
