package lndclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LightningClient exposes base lightning functionality.
type LightningClient interface {
	PayInvoice(ctx context.Context, invoice string,
		maxFee btcutil.Amount,
		outgoingChannel *uint64) chan PaymentResult

	GetInfo(ctx context.Context) (*Info, error)

	EstimateFeeToP2WSH(ctx context.Context, amt btcutil.Amount,
		confTarget int32) (btcutil.Amount, error)

	ConfirmedWalletBalance(ctx context.Context) (btcutil.Amount, error)

	AddInvoice(ctx context.Context, in *invoicesrpc.AddInvoiceData) (
		lntypes.Hash, string, error)

	// LookupInvoice looks up an invoice by hash.
	LookupInvoice(ctx context.Context, hash lntypes.Hash) (*Invoice, error)

	// ListTransactions returns all known transactions of the backing lnd
	// node. It takes a start and end block height which can be used to
	// limit the block range that we query over. These values can be left
	// as zero to include all blocks. To include unconfirmed transactions
	// in the query, endHeight must be set to -1.
	ListTransactions(ctx context.Context, startHeight,
		endHeight int32) ([]Transaction, error)

	// ListChannels retrieves all channels of the backing lnd node.
	ListChannels(ctx context.Context) ([]ChannelInfo, error)

	// PendingChannels returns a list of lnd's pending channels.
	PendingChannels(ctx context.Context) (*PendingChannels, error)

	// ClosedChannels returns all closed channels of the backing lnd node.
	ClosedChannels(ctx context.Context) ([]ClosedChannel, error)

	// ForwardingHistory makes a paginated call to our forwarding history
	// endpoint.
	ForwardingHistory(ctx context.Context,
		req ForwardingHistoryRequest) (*ForwardingHistoryResponse, error)

	// ListInvoices makes a paginated call to our list invoices endpoint.
	ListInvoices(ctx context.Context, req ListInvoicesRequest) (
		*ListInvoicesResponse, error)

	// ListPayments makes a paginated call to our list payments endpoint.
	ListPayments(ctx context.Context,
		req ListPaymentsRequest) (*ListPaymentsResponse, error)

	// ChannelBackup retrieves the backup for a particular channel. The
	// backup is returned as an encrypted chanbackup.Single payload.
	ChannelBackup(context.Context, wire.OutPoint) ([]byte, error)

	// ChannelBackups retrieves backups for all existing pending open and
	// open channels. The backups are returned as an encrypted
	// chanbackup.Multi payload.
	ChannelBackups(ctx context.Context) ([]byte, error)

	// DecodePaymentRequest decodes a payment request.
	DecodePaymentRequest(ctx context.Context,
		payReq string) (*PaymentRequest, error)

	// OpenChannel opens a channel to the peer provided with the amounts
	// specified.
	OpenChannel(ctx context.Context, peer route.Vertex,
		localSat, pushSat btcutil.Amount) (*wire.OutPoint, error)

	// CloseChannel closes the channel provided.
	CloseChannel(ctx context.Context, channel *wire.OutPoint,
		force bool) (chan CloseChannelUpdate, chan error, error)

	// Connect attempts to connect to a peer at the host specified.
	Connect(ctx context.Context, peer route.Vertex, host string) error
}

// Info contains info about the connected lnd node.
type Info struct {
	BlockHeight    uint32
	IdentityPubkey [33]byte
	Alias          string
	Network        string
	Uris           []string

	// SyncedToChain is true if the wallet's view is synced to the main
	// chain.
	SyncedToChain bool

	// SyncedToGraph is true if we consider ourselves to be synced with the
	// public channel graph.
	SyncedToGraph bool
}

// ChannelInfo stores unpacked per-channel info.
type ChannelInfo struct {
	// ChannelPoint is the funding outpoint of the channel.
	ChannelPoint string

	// Active indicates whether the channel is active.
	Active bool

	// ChannelID holds the unique channel ID for the channel. The first 3 bytes
	// are the block height, the next 3 the index within the block, and the last
	// 2 bytes are the /output index for the channel.
	ChannelID uint64

	// PubKeyBytes is the raw bytes of the public key of the remote node.
	PubKeyBytes route.Vertex

	// Capacity is the total amount of funds held in this channel.
	Capacity btcutil.Amount

	// LocalBalance is the current balance of this node in this channel.
	LocalBalance btcutil.Amount

	// RemoteBalance is the counterparty's current balance in this channel.
	RemoteBalance btcutil.Amount

	// Initiator indicates whether we opened the channel or not.
	Initiator bool

	// Private indicates that the channel is private.
	Private bool

	// LifeTime is the total amount of time we have monitored the peer's
	// online status for.
	LifeTime time.Duration

	// Uptime is the total amount of time the peer has been observed as
	// online over its lifetime.
	Uptime time.Duration
}

// ClosedChannel represents a channel that has been closed.
type ClosedChannel struct {
	// ChannelPoint is the funding outpoint of the channel.
	ChannelPoint string

	// ChannelID holds the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// ClosingTxHash is the tx hash of the close transaction for the channel.
	ClosingTxHash string

	// CloseType is the type of channel closure.
	CloseType CloseType

	// OpenInitiator is true if we opened the channel. This value is not
	// always available (older channels do not have it).
	OpenInitiator Initiator

	// Initiator indicates which party initiated the channel close. Since
	// this value is not always set in the rpc response, we also make a best
	// effort attempt to set it based on CloseType.
	CloseInitiator Initiator

	// PubKeyBytes is the raw bytes of the public key of the remote node.
	PubKeyBytes route.Vertex

	// Capacity is the total amount of funds held in this channel.
	Capacity btcutil.Amount

	// SettledBalance is the amount we were paid out directly in this
	// channel close. Note that this does not include cases where we need to
	// sweep our commitment or htlcs.
	SettledBalance btcutil.Amount
}

// CloseType is an enum which represents the types of closes our channels may
// have. This type maps to the rpc value.
type CloseType uint8

const (
	// CloseTypeCooperative represents cooperative closes.
	CloseTypeCooperative CloseType = iota

	// CloseTypeLocalForce represents force closes that we initiated.
	CloseTypeLocalForce

	// CloseTypeRemoteForce represents force closes that our peer initiated.
	CloseTypeRemoteForce

	// CloseTypeBreach represents breach closes from our peer.
	CloseTypeBreach

	// CloseTypeFundingCancelled represents channels which were never
	// created because their funding transaction was cancelled.
	CloseTypeFundingCancelled

	// CloseTypeAbandoned represents a channel that was abandoned.
	CloseTypeAbandoned
)

// String returns the string representation of a close type.
func (c CloseType) String() string {
	switch c {
	case CloseTypeCooperative:
		return "Cooperative"

	case CloseTypeLocalForce:
		return "Local Force"

	case CloseTypeRemoteForce:
		return "Remote Force"

	case CloseTypeBreach:
		return "Breach"

	case CloseTypeFundingCancelled:
		return "Funding Cancelled"

	case CloseTypeAbandoned:
		return "Abandoned"

	default:
		return "Unknown"
	}
}

// Initiator indicates the party that opened or closed a channel. This enum is
// used for cases where we may not have a full set of initiator information
// available over rpc (this is the case for older channels).
type Initiator uint8

const (
	// InitiatorUnrecorded is set when we do not know the open/close
	// initiator for a channel, this is the case when the channel was
	// closed before lnd started tracking initiators.
	InitiatorUnrecorded Initiator = iota

	// InitiatorLocal is set when we initiated a channel open or close.
	InitiatorLocal

	// InitiatorRemote is set when the remote party initiated a chanel open
	// or close.
	InitiatorRemote

	// InitiatorBoth is set in the case where both parties initiated a
	// cooperative close (this is possible with multiple rounds of
	// negotiation).
	InitiatorBoth
)

// String provides the string represenetation of a close initiator.
func (c Initiator) String() string {
	switch c {
	case InitiatorUnrecorded:
		return "Unrecorded"

	case InitiatorLocal:
		return "Local"

	case InitiatorRemote:
		return "Remote"

	case InitiatorBoth:
		return "Both"

	default:
		return fmt.Sprintf("unknown initiator: %d", c)
	}
}

// Transaction represents an on chain transaction.
type Transaction struct {
	// Tx is the on chain transaction.
	Tx *wire.MsgTx

	// TxHash is the transaction hash string.
	TxHash string

	// Timestamp is the timestamp our wallet has for the transaction.
	Timestamp time.Time

	// Amount is the balance change that this transaction had on addresses
	// controlled by our wallet.
	Amount btcutil.Amount

	// Fee is the amount of fees our wallet committed to this transaction.
	// Note that this field is not exhaustive, as it does not account for
	// fees taken from inputs that that wallet doesn't know it owns (for
	// example, the fees taken from our channel balance when we close a
	// channel).
	Fee btcutil.Amount

	// Confirmations is the number of confirmations the transaction has.
	Confirmations int32

	// Label is an optional label set for on chain transactions.
	Label string
}

var (
	// ErrMalformedServerResponse is returned when the swap and/or prepay
	// invoice is malformed.
	ErrMalformedServerResponse = errors.New(
		"one or more invoices are malformed",
	)

	// ErrNoRouteToServer is returned if no quote can returned because there
	// is no route to the server.
	ErrNoRouteToServer = errors.New("no off-chain route to server")

	// PaymentResultUnknownPaymentHash is the string result returned by
	// SendPayment when the final node indicates the hash is unknown.
	PaymentResultUnknownPaymentHash = "UnknownPaymentHash"

	// PaymentResultSuccess is the string result returned by SendPayment
	// when the payment was successful.
	PaymentResultSuccess = ""

	// PaymentResultAlreadyPaid is the string result returned by SendPayment
	// when the payment was already completed in a previous SendPayment
	// call.
	PaymentResultAlreadyPaid = channeldb.ErrAlreadyPaid.Error()

	// PaymentResultInFlight is the string result returned by SendPayment
	// when the payment was initiated in a previous SendPayment call and
	// still in flight.
	PaymentResultInFlight = channeldb.ErrPaymentInFlight.Error()

	paymentPollInterval = 3 * time.Second
)

type lightningClient struct {
	client   lnrpc.LightningClient
	wg       sync.WaitGroup
	params   *chaincfg.Params
	adminMac serializedMacaroon
}

func newLightningClient(conn *grpc.ClientConn,
	params *chaincfg.Params, adminMac serializedMacaroon) *lightningClient {

	return &lightningClient{
		client:   lnrpc.NewLightningClient(conn),
		params:   params,
		adminMac: adminMac,
	}
}

// PaymentResult signals the result of a payment.
type PaymentResult struct {
	Err      error
	Preimage lntypes.Preimage
	PaidFee  btcutil.Amount
	PaidAmt  btcutil.Amount
}

func (s *lightningClient) WaitForFinished() {
	s.wg.Wait()
}

func (s *lightningClient) ConfirmedWalletBalance(ctx context.Context) (
	btcutil.Amount, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.WalletBalance(rpcCtx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		return 0, err
	}

	return btcutil.Amount(resp.ConfirmedBalance), nil
}

func (s *lightningClient) GetInfo(ctx context.Context) (*Info, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.GetInfo(rpcCtx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, err
	}

	pubKey, err := hex.DecodeString(resp.IdentityPubkey)
	if err != nil {
		return nil, err
	}

	var pubKeyArray [33]byte
	copy(pubKeyArray[:], pubKey)

	return &Info{
		BlockHeight:    resp.BlockHeight,
		IdentityPubkey: pubKeyArray,
		Alias:          resp.Alias,
		Network:        resp.Chains[0].Network,
		Uris:           resp.Uris,
		SyncedToChain:  resp.SyncedToChain,
		SyncedToGraph:  resp.SyncedToGraph,
	}, nil
}

func (s *lightningClient) EstimateFeeToP2WSH(ctx context.Context,
	amt btcutil.Amount, confTarget int32) (btcutil.Amount,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// Generate dummy p2wsh address for fee estimation.
	wsh := [32]byte{}
	p2wshAddress, err := btcutil.NewAddressWitnessScriptHash(
		wsh[:], s.params,
	)
	if err != nil {
		return 0, err
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.EstimateFee(
		rpcCtx,
		&lnrpc.EstimateFeeRequest{
			TargetConf: confTarget,
			AddrToAmount: map[string]int64{
				p2wshAddress.String(): int64(amt),
			},
		},
	)
	if err != nil {
		return 0, err
	}
	return btcutil.Amount(resp.FeeSat), nil
}

// PayInvoice pays an invoice.
func (s *lightningClient) PayInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount, outgoingChannel *uint64) chan PaymentResult {

	// Use buffer to prevent blocking.
	paymentChan := make(chan PaymentResult, 1)

	// Execute payment in parallel, because it will block until server
	// discovers preimage.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		result := s.payInvoice(ctx, invoice, maxFee, outgoingChannel)
		if result != nil {
			paymentChan <- *result
		}
	}()

	return paymentChan
}

// payInvoice tries to send a payment and returns the final result. If
// necessary, it will poll lnd for the payment result.
func (s *lightningClient) payInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount, outgoingChannel *uint64) *PaymentResult {

	payReq, err := zpay32.Decode(invoice, s.params)
	if err != nil {
		return &PaymentResult{
			Err: fmt.Errorf("invoice decode: %v", err),
		}
	}

	if payReq.MilliSat == nil {
		return &PaymentResult{
			Err: errors.New("no amount in invoice"),
		}
	}

	hash := lntypes.Hash(*payReq.PaymentHash)

	ctx = s.adminMac.WithMacaroonAuth(ctx)
	for {
		// Create no timeout context as this call can block for a long
		// time.

		req := &lnrpc.SendRequest{
			FeeLimit: &lnrpc.FeeLimit{
				Limit: &lnrpc.FeeLimit_Fixed{
					Fixed: int64(maxFee),
				},
			},
			PaymentRequest: invoice,
		}

		if outgoingChannel != nil {
			req.OutgoingChanId = *outgoingChannel
		}

		payResp, err := s.client.SendPaymentSync(ctx, req)

		if status.Code(err) == codes.Canceled {
			return nil
		}

		if err == nil {
			// TODO: Use structured payment error when available,
			// instead of this britle string matching.
			switch payResp.PaymentError {

			// Paid successfully.
			case PaymentResultSuccess:
				log.Infof(
					"Payment %v completed", hash,
				)

				r := payResp.PaymentRoute
				preimage, err := lntypes.MakePreimage(
					payResp.PaymentPreimage,
				)
				if err != nil {
					return &PaymentResult{Err: err}
				}
				return &PaymentResult{
					PaidFee: btcutil.Amount(r.TotalFees),
					PaidAmt: btcutil.Amount(
						r.TotalAmt - r.TotalFees,
					),
					Preimage: preimage,
				}

			// Invoice was already paid on a previous run.
			case PaymentResultAlreadyPaid:
				log.Infof(
					"Payment %v already completed", hash,
				)

				// Unfortunately lnd doesn't return the route if
				// the payment was successful in a previous
				// call. Assume paid fees 0 and take paid amount
				// from invoice.

				return &PaymentResult{
					PaidFee: 0,
					PaidAmt: payReq.MilliSat.ToSatoshis(),
				}

			// If the payment is already in flight, we will poll
			// again later for an outcome.
			//
			// TODO: Improve this when lnd expose more API to
			// tracking existing payments.
			case PaymentResultInFlight:
				log.Infof(
					"Payment %v already in flight", hash,
				)

				time.Sleep(paymentPollInterval)

			// Other errors are transformed into an error struct.
			default:
				log.Warnf(
					"Payment %v failed: %v", hash,
					payResp.PaymentError,
				)

				return &PaymentResult{
					Err: errors.New(payResp.PaymentError),
				}
			}
		}
	}
}

func (s *lightningClient) AddInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (lntypes.Hash, string, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &lnrpc.Invoice{
		Memo:       in.Memo,
		Value:      int64(in.Value.ToSatoshis()),
		Expiry:     in.Expiry,
		CltvExpiry: in.CltvExpiry,
		Private:    true,
	}

	if in.Preimage != nil {
		rpcIn.RPreimage = in.Preimage[:]
	}
	if in.Hash != nil {
		rpcIn.RHash = in.Hash[:]
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.AddInvoice(rpcCtx, rpcIn)
	if err != nil {
		return lntypes.Hash{}, "", err
	}
	hash, err := lntypes.MakeHash(resp.RHash)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	return hash, resp.PaymentRequest, nil
}

// Invoice represents an invoice in lnd.
type Invoice struct {
	// Preimage is the invoice's preimage, which is set if the invoice
	// is settled.
	Preimage *lntypes.Preimage

	// Hash is the invoice hash.
	Hash lntypes.Hash

	// Memo is an optional memo field for hte invoice.
	Memo string

	// PaymentRequest is the invoice's payment request.
	PaymentRequest string

	// Amount is the amount of the invoice in millisatoshis.
	Amount lnwire.MilliSatoshi

	// AmountPaid is the amount that was paid for the invoice. This field
	// will only be set if the invoice is settled.
	AmountPaid lnwire.MilliSatoshi

	// CreationDate is the time the invoice was created.
	CreationDate time.Time

	// SettleDate is the time the invoice was settled.
	SettleDate time.Time

	// State is the invoice's current state.
	State channeldb.ContractState

	// IsKeysend indicates whether the invoice was a spontaneous payment.
	IsKeysend bool
}

// LookupInvoice looks up an invoice in lnd, it will error if the invoice is
// not known to lnd.
func (s *lightningClient) LookupInvoice(ctx context.Context,
	hash lntypes.Hash) (*Invoice, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &lnrpc.PaymentHash{
		RHash: hash[:],
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.LookupInvoice(rpcCtx, rpcIn)
	if err != nil {
		return nil, err
	}

	invoice, err := unmarshalInvoice(resp)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// unmarshalInvoice creates an invoice from the rpc response provided.
func unmarshalInvoice(resp *lnrpc.Invoice) (*Invoice, error) {
	hash, err := lntypes.MakeHash(resp.RHash)
	if err != nil {
		return nil, err
	}

	invoice := &Invoice{
		Preimage:       nil,
		Hash:           hash,
		Memo:           resp.Memo,
		PaymentRequest: resp.PaymentRequest,
		Amount:         lnwire.MilliSatoshi(resp.ValueMsat),
		AmountPaid:     lnwire.MilliSatoshi(resp.AmtPaidMsat),
		CreationDate:   time.Unix(resp.CreationDate, 0),
		IsKeysend:      resp.IsKeysend,
	}

	switch resp.State {
	case lnrpc.Invoice_OPEN:
		invoice.State = channeldb.ContractOpen

	case lnrpc.Invoice_ACCEPTED:
		invoice.State = channeldb.ContractAccepted

	// If the invoice is settled, it also has a non-nil preimage, which we
	// can set on our invoice.
	case lnrpc.Invoice_SETTLED:
		invoice.State = channeldb.ContractSettled
		preimage, err := lntypes.MakePreimage(resp.RPreimage)
		if err != nil {
			return nil, err
		}
		invoice.Preimage = &preimage

	case lnrpc.Invoice_CANCELED:
		invoice.State = channeldb.ContractCanceled

	default:
		return nil, fmt.Errorf("unknown invoice state: %v",
			resp.State)
	}

	// Only set settle date if it is non-zero, because 0 unix time is
	// not the same as a zero time struct.
	if resp.SettleDate != 0 {
		invoice.SettleDate = time.Unix(resp.SettleDate, 0)
	}

	return invoice, nil
}

// ListTransactions returns all known transactions of the backing lnd node.
func (s *lightningClient) ListTransactions(ctx context.Context, startHeight,
	endHeight int32) ([]Transaction, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	rpcIn := &lnrpc.GetTransactionsRequest{
		StartHeight: startHeight,
		EndHeight:   endHeight,
	}

	resp, err := s.client.GetTransactions(rpcCtx, rpcIn)
	if err != nil {
		return nil, err
	}

	txs := make([]Transaction, len(resp.Transactions))
	for i, respTx := range resp.Transactions {
		rawTx, err := hex.DecodeString(respTx.RawTxHex)
		if err != nil {
			return nil, err
		}

		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
			return nil, err
		}

		txs[i] = Transaction{
			Tx:            &tx,
			TxHash:        tx.TxHash().String(),
			Timestamp:     time.Unix(respTx.TimeStamp, 0),
			Amount:        btcutil.Amount(respTx.Amount),
			Fee:           btcutil.Amount(respTx.TotalFees),
			Confirmations: respTx.NumConfirmations,
			Label:         respTx.Label,
		}
	}

	return txs, nil
}

// ListChannels retrieves all channels of the backing lnd node.
func (s *lightningClient) ListChannels(ctx context.Context) (
	[]ChannelInfo, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	response, err := s.client.ListChannels(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return nil, err
	}

	result := make([]ChannelInfo, len(response.Channels))
	for i, channel := range response.Channels {
		remoteVertex, err := route.NewVertexFromStr(channel.RemotePubkey)
		if err != nil {
			return nil, err
		}

		result[i] = ChannelInfo{
			ChannelPoint:  channel.ChannelPoint,
			Active:        channel.Active,
			ChannelID:     channel.ChanId,
			PubKeyBytes:   remoteVertex,
			Capacity:      btcutil.Amount(channel.Capacity),
			LocalBalance:  btcutil.Amount(channel.LocalBalance),
			RemoteBalance: btcutil.Amount(channel.RemoteBalance),
			Initiator:     channel.Initiator,
			Private:       channel.Private,
			LifeTime: time.Second * time.Duration(
				channel.Lifetime,
			),
			Uptime: time.Second * time.Duration(
				channel.Uptime,
			),
		}
	}

	return result, nil
}

// PendingChannels contains lnd's channels that are pending open and close.
type PendingChannels struct {
	// PendingForceClose contains our channels that have been force closed,
	// and are awaiting full on chain resolution.
	PendingForceClose []ForceCloseChannel

	// PendingOpen contains channels that we are waiting to confirm on chain
	// so that they can be marked as fully open.
	PendingOpen []PendingChannel

	// WaitingClose contains channels that are waiting for their close tx
	// to confirm.
	WaitingClose []WaitingCloseChannel
}

// PendingChannel contains the information common to all pending channels.
type PendingChannel struct {
	// ChannelPoint is the outpoint of the channel.
	ChannelPoint *wire.OutPoint

	// PubKeyBytes is the raw bytes of the public key of the remote node.
	PubKeyBytes route.Vertex

	// Capacity is the total amount of funds held in this channel.
	Capacity btcutil.Amount

	// ChannelInitiator indicates which party opened the channel.
	ChannelInitiator Initiator
}

// NewPendingChannel creates a pending channel from the rpc struct.
func NewPendingChannel(channel *lnrpc.PendingChannelsResponse_PendingChannel) (
	*PendingChannel, error) {

	peer, err := route.NewVertexFromStr(channel.RemoteNodePub)
	if err != nil {
		return nil, err
	}

	outpoint, err := NewOutpointFromStr(channel.ChannelPoint)
	if err != nil {
		return nil, err
	}

	initiator, err := getInitiator(channel.Initiator)
	if err != nil {
		return nil, err
	}

	return &PendingChannel{
		ChannelPoint:     outpoint,
		PubKeyBytes:      peer,
		Capacity:         btcutil.Amount(channel.Capacity),
		ChannelInitiator: initiator,
	}, nil
}

// ForceCloseChannel describes a channel that pending force close.
type ForceCloseChannel struct {
	// PendingChannel contains information about the channel.
	PendingChannel

	// CloseTxid is the close transaction that confirmed on chain.
	CloseTxid chainhash.Hash
}

// WaitingCloseChannel describes a channel that we are waiting to be closed on
// chain. It contains both parties close txids because either may confirm at
// this point.
type WaitingCloseChannel struct {
	// PendingChannel contains information about the channel.
	PendingChannel

	// LocalTxid is our close transaction's txid.
	LocalTxid chainhash.Hash

	// RemoteTxid is the remote party's close txid.
	RemoteTxid chainhash.Hash

	// RemotePending is the txid of the remote party's pending commit.
	RemotePending chainhash.Hash
}

// PendingChannels returns a list of lnd's pending channels.
func (s *lightningClient) PendingChannels(ctx context.Context) (*PendingChannels,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	resp, err := s.client.PendingChannels(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.PendingChannelsRequest{},
	)
	if err != nil {
		return nil, err
	}

	pending := &PendingChannels{
		PendingForceClose: make([]ForceCloseChannel, len(resp.PendingForceClosingChannels)),
		PendingOpen:       make([]PendingChannel, len(resp.PendingOpenChannels)),
		WaitingClose:      make([]WaitingCloseChannel, len(resp.WaitingCloseChannels)),
	}

	for i, force := range resp.PendingForceClosingChannels {
		channel, err := NewPendingChannel(force.Channel)
		if err != nil {
			return nil, err
		}

		hash, err := chainhash.NewHashFromStr(force.ClosingTxid)
		if err != nil {
			return nil, err
		}

		pending.PendingForceClose[i] = ForceCloseChannel{
			PendingChannel: *channel,
			CloseTxid:      *hash,
		}
	}

	for i, waiting := range resp.WaitingCloseChannels {
		channel, err := NewPendingChannel(waiting.Channel)
		if err != nil {
			return nil, err
		}

		local, err := chainhash.NewHashFromStr(
			waiting.Commitments.LocalTxid,
		)
		if err != nil {
			return nil, err
		}

		remote, err := chainhash.NewHashFromStr(
			waiting.Commitments.RemoteTxid,
		)
		if err != nil {
			return nil, err
		}

		remotePending, err := chainhash.NewHashFromStr(
			waiting.Commitments.RemotePendingTxid,
		)
		if err != nil {
			return nil, err
		}

		closing := WaitingCloseChannel{
			PendingChannel: *channel,
			LocalTxid:      *local,
			RemoteTxid:     *remote,
			RemotePending:  *remotePending,
		}
		pending.WaitingClose[i] = closing
	}

	for i, open := range resp.PendingOpenChannels {
		channel, err := NewPendingChannel(open.Channel)
		if err != nil {
			return nil, err
		}

		pending.PendingOpen[i] = *channel
	}

	return pending, nil
}

// ClosedChannels returns a list of our closed channels.
func (s *lightningClient) ClosedChannels(ctx context.Context) ([]ClosedChannel,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	response, err := s.client.ClosedChannels(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.ClosedChannelsRequest{},
	)
	if err != nil {
		return nil, err
	}

	channels := make([]ClosedChannel, len(response.Channels))
	for i, channel := range response.Channels {
		remote, err := route.NewVertexFromStr(channel.RemotePubkey)
		if err != nil {
			return nil, err
		}

		closeType, err := rpcCloseType(channel.CloseType)
		if err != nil {
			return nil, err
		}

		openInitiator, err := getInitiator(channel.OpenInitiator)
		if err != nil {
			return nil, err
		}

		closeInitiator, err := rpcCloseInitiator(
			channel.CloseInitiator, closeType,
		)
		if err != nil {
			return nil, err
		}

		channels[i] = ClosedChannel{
			ChannelPoint:   channel.ChannelPoint,
			ChannelID:      channel.ChanId,
			ClosingTxHash:  channel.ClosingTxHash,
			CloseType:      closeType,
			OpenInitiator:  openInitiator,
			CloseInitiator: closeInitiator,
			PubKeyBytes:    remote,
			Capacity:       btcutil.Amount(channel.Capacity),
			SettledBalance: btcutil.Amount(channel.SettledBalance),
		}
	}

	return channels, nil
}

// rpcCloseType maps a rpc close type to our local enum.
func rpcCloseType(t lnrpc.ChannelCloseSummary_ClosureType) (CloseType, error) {
	switch t {
	case lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE:
		return CloseTypeCooperative, nil

	case lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE:
		return CloseTypeLocalForce, nil

	case lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE:
		return CloseTypeRemoteForce, nil

	case lnrpc.ChannelCloseSummary_BREACH_CLOSE:
		return CloseTypeBreach, nil

	case lnrpc.ChannelCloseSummary_FUNDING_CANCELED:
		return CloseTypeFundingCancelled, nil

	case lnrpc.ChannelCloseSummary_ABANDONED:
		return CloseTypeAbandoned, nil

	default:
		return 0, fmt.Errorf("unknown close type: %v", t)
	}
}

// rpcCloseInitiator maps a close initiator to our local type. Since this field
// is not always set in lnd for older channels, also use our close type to infer
// who initiated the close when we have force closes.
func rpcCloseInitiator(initiator lnrpc.Initiator,
	closeType CloseType) (Initiator, error) {

	// Since our close type is always set on the rpc, we first check whether
	// we can figure out the close initiator from this value. This is only
	// possible for force closes/breaches.
	switch closeType {
	case CloseTypeLocalForce:
		return InitiatorLocal, nil

	case CloseTypeRemoteForce, CloseTypeBreach:
		return InitiatorRemote, nil
	}

	// Otherwise, we check whether our initiator field is set, and fail only
	// if we have an unknown type.
	return getInitiator(initiator)
}

// getInitiator maps a rpc initiator value to our initiator enum.
func getInitiator(initiator lnrpc.Initiator) (Initiator, error) {
	switch initiator {
	case lnrpc.Initiator_INITIATOR_LOCAL:
		return InitiatorLocal, nil

	case lnrpc.Initiator_INITIATOR_REMOTE:
		return InitiatorRemote, nil

	case lnrpc.Initiator_INITIATOR_BOTH:
		return InitiatorBoth, nil

	case lnrpc.Initiator_INITIATOR_UNKNOWN:
		return InitiatorUnrecorded, nil

	default:
		return InitiatorUnrecorded, fmt.Errorf("unknown "+
			"initiator: %v", initiator)
	}
}

// ForwardingHistoryRequest contains the request parameters for a paginated
// forwarding history call.
type ForwardingHistoryRequest struct {
	// StartTime is the beginning of the query period.
	StartTime time.Time

	// EndTime is the end of the query period.
	EndTime time.Time

	// MaxEvents is the maximum number of events to return.
	MaxEvents uint32

	// Offset is the index from which to start querying.
	Offset uint32
}

// ForwardingHistoryResponse contains the response to a forwarding history
// query, including last index offset required for paginated queries.
type ForwardingHistoryResponse struct {
	// LastIndexOffset is the index offset of the last item in our set.
	LastIndexOffset uint32

	// Events is the set of events that were found in the interval queried.
	Events []ForwardingEvent
}

// ForwardingEvent represents a htlc that was forwarded through our node.
type ForwardingEvent struct {
	// Timestamp is the time that we processed the forwarding event.
	Timestamp time.Time

	// ChannelIn is the id of the channel the htlc arrived at our node on.
	ChannelIn uint64

	// ChannelOut is the id of the channel the htlc left our node on.
	ChannelOut uint64

	// AmountMsatIn is the amount that was forwarded into our node in
	// millisatoshis.
	AmountMsatIn lnwire.MilliSatoshi

	// AmountMsatOut is the amount that was forwarded out of our node in
	// millisatoshis.
	AmountMsatOut lnwire.MilliSatoshi

	// FeeMsat is the amount of fees earned in millisatoshis,
	FeeMsat lnwire.MilliSatoshi
}

// ForwardingHistory returns a set of forwarding events for the period queried.
// Note that this call is paginated, and the information required to make
// subsequent calls is provided in the response.
func (s *lightningClient) ForwardingHistory(ctx context.Context,
	req ForwardingHistoryRequest) (*ForwardingHistoryResponse, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	response, err := s.client.ForwardingHistory(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.ForwardingHistoryRequest{
			StartTime:    uint64(req.StartTime.Unix()),
			EndTime:      uint64(req.EndTime.Unix()),
			IndexOffset:  req.Offset,
			NumMaxEvents: req.MaxEvents,
		},
	)
	if err != nil {
		return nil, err
	}

	events := make([]ForwardingEvent, len(response.ForwardingEvents))
	for i, event := range response.ForwardingEvents {
		events[i] = ForwardingEvent{
			Timestamp:     time.Unix(int64(event.Timestamp), 0),
			ChannelIn:     event.ChanIdIn,
			ChannelOut:    event.ChanIdOut,
			AmountMsatIn:  lnwire.MilliSatoshi(event.AmtIn),
			AmountMsatOut: lnwire.MilliSatoshi(event.AmtOut),
			FeeMsat:       lnwire.MilliSatoshi(event.FeeMsat),
		}
	}

	return &ForwardingHistoryResponse{
		LastIndexOffset: response.LastOffsetIndex,
		Events:          events,
	}, nil
}

// ListInvoicesRequest contains the request parameters for a paginated
// list invoices call.
type ListInvoicesRequest struct {
	// MaxInvoices is the maximum number of invoices to return.
	MaxInvoices uint64

	// Offset is the index from which to start querying.
	Offset uint64

	// Reversed is set to query our invoices backwards.
	Reversed bool

	// PendingOnly is set if we only want pending invoices.
	PendingOnly bool
}

// ListInvoicesResponse contains the response to a list invoices query,
// including the index offsets required for paginated queries.
type ListInvoicesResponse struct {
	// FirstIndexOffset is the index offset of the first item in our set.
	FirstIndexOffset uint64

	// LastIndexOffset is the index offset of the last item in our set.
	LastIndexOffset uint64

	// Invoices is the set of invoices that were returned.
	Invoices []Invoice
}

// ListInvoices returns a list of invoices from our node.
func (s *lightningClient) ListInvoices(ctx context.Context,
	req ListInvoicesRequest) (*ListInvoicesResponse, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	resp, err := s.client.ListInvoices(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.ListInvoiceRequest{
			PendingOnly:    false,
			IndexOffset:    req.Offset,
			NumMaxInvoices: req.MaxInvoices,
			Reversed:       req.Reversed,
		},
	)
	if err != nil {
		return nil, err
	}

	invoices := make([]Invoice, len(resp.Invoices))
	for i, invoice := range resp.Invoices {
		inv, err := unmarshalInvoice(invoice)
		if err != nil {
			return nil, err
		}

		invoices[i] = *inv
	}

	return &ListInvoicesResponse{
		FirstIndexOffset: resp.FirstIndexOffset,
		LastIndexOffset:  resp.LastIndexOffset,
		Invoices:         invoices,
	}, nil
}

// Payment represents a payment made by our node.
type Payment struct {
	// Hash is the payment hash used.
	Hash lntypes.Hash

	// Preimage is the preimage of the payment. It will have a non-nil value
	// if the payment is settled.
	Preimage *lntypes.Preimage

	// PaymentRequest is the payment request for this payment. This value
	// will not be set for keysend payments and for payments that manually
	// specify their destination and amount.
	PaymentRequest string

	// Amount is the amount in millisatoshis of the payment.
	Amount lnwire.MilliSatoshi

	// Fee is the amount in millisatoshis that was paid in fees.
	Fee lnwire.MilliSatoshi

	// Status describes the state of a payment.
	Status *PaymentStatus

	// Htlcs is the set of htlc attempts made by the payment.
	Htlcs []*lnrpc.HTLCAttempt

	// SequenceNumber is a unique id for each payment.
	SequenceNumber uint64
}

// ListPaymentsRequest contains the request parameters for a paginated
// list payments call.
type ListPaymentsRequest struct {
	// MaxPayments is the maximum number of payments to return.
	MaxPayments uint64

	// Offset is the index from which to start querying.
	Offset uint64

	// Reversed is set to query our payments backwards.
	Reversed bool

	// IncludeIncomplete is set if we want to include incomplete payments.
	IncludeIncomplete bool
}

// ListPaymentsResponse contains the response to a list payments query,
// including the index offsets required for paginated queries.
type ListPaymentsResponse struct {
	// FirstIndexOffset is the index offset of the first item in our set.
	FirstIndexOffset uint64

	// LastIndexOffset is the index offset of the last item in our set.
	LastIndexOffset uint64

	// Payments is the set of invoices that were returned.
	Payments []Payment
}

// ListPayments makes a paginated call to our listpayments endpoint.
func (s *lightningClient) ListPayments(ctx context.Context,
	req ListPaymentsRequest) (*ListPaymentsResponse, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	resp, err := s.client.ListPayments(
		s.adminMac.WithMacaroonAuth(rpcCtx),
		&lnrpc.ListPaymentsRequest{
			IncludeIncomplete: req.IncludeIncomplete,
			IndexOffset:       req.Offset,
			MaxPayments:       req.MaxPayments,
			Reversed:          req.Reversed,
		})
	if err != nil {
		return nil, err
	}

	payments := make([]Payment, len(resp.Payments))
	for i, payment := range resp.Payments {
		hash, err := lntypes.MakeHashFromStr(payment.PaymentHash)
		if err != nil {
			return nil, err
		}

		status, err := unmarshallPaymentStatus(payment)
		if err != nil {
			return nil, err
		}

		pmt := Payment{
			Hash:           hash,
			PaymentRequest: payment.PaymentRequest,
			Status:         status,
			Htlcs:          payment.Htlcs,
			Amount:         lnwire.MilliSatoshi(payment.ValueMsat),
			Fee:            lnwire.MilliSatoshi(payment.FeeMsat),
			SequenceNumber: payment.PaymentIndex,
		}

		// Add our preimage if it is known.
		if payment.PaymentPreimage != "" {
			preimage, err := lntypes.MakePreimageFromStr(
				payment.PaymentPreimage,
			)
			if err != nil {
				return nil, err
			}
			pmt.Preimage = &preimage
		}

		payments[i] = pmt
	}

	return &ListPaymentsResponse{
		FirstIndexOffset: resp.FirstIndexOffset,
		LastIndexOffset:  resp.LastIndexOffset,
		Payments:         payments,
	}, nil
}

// ChannelBackup retrieves the backup for a particular channel. The backup is
// returned as an encrypted chanbackup.Single payload.
func (s *lightningClient) ChannelBackup(ctx context.Context,
	channelPoint wire.OutPoint) ([]byte, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	req := &lnrpc.ExportChannelBackupRequest{
		ChanPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: channelPoint.Hash[:],
			},
			OutputIndex: channelPoint.Index,
		},
	}
	resp, err := s.client.ExportChannelBackup(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	return resp.ChanBackup, nil
}

// ChannelBackups retrieves backups for all existing pending open and open
// channels. The backups are returned as an encrypted chanbackup.Multi payload.
func (s *lightningClient) ChannelBackups(ctx context.Context) ([]byte, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	req := &lnrpc.ChanBackupExportRequest{}
	resp, err := s.client.ExportAllChannelBackups(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	return resp.MultiChanBackup.MultiChanBackup, nil
}

// PaymentRequest represents a request for payment from a node.
type PaymentRequest struct {
	// Destination is the node that this payment request pays to .
	Destination route.Vertex

	// Hash is the payment hash associated with this payment
	Hash lntypes.Hash

	// Value is the value of the payment request in millisatoshis.
	Value lnwire.MilliSatoshi

	/// Timestamp of the payment request.
	Timestamp time.Time

	// Expiry is the time at which the payment request expires.
	Expiry time.Time

	// Description is a description attached to the payment request.
	Description string

	// PaymentAddress is the payment address associated with the invoice,
	// set if the receiver supports mpp.
	PaymentAddress [32]byte
}

// DecodePaymentRequest decodes a payment request.
func (s *lightningClient) DecodePaymentRequest(ctx context.Context,
	payReq string) (*PaymentRequest, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	resp, err := s.client.DecodePayReq(rpcCtx, &lnrpc.PayReqString{
		PayReq: payReq,
	})
	if err != nil {
		return nil, err
	}

	hash, err := lntypes.MakeHashFromStr(resp.PaymentHash)
	if err != nil {
		return nil, err
	}

	dest, err := route.NewVertexFromStr(resp.Destination)
	if err != nil {
		return nil, err
	}

	paymentReq := &PaymentRequest{
		Destination: dest,
		Hash:        hash,
		Value:       lnwire.MilliSatoshi(resp.NumMsat),
		Description: resp.Description,
	}

	copy(paymentReq.PaymentAddress[:], resp.PaymentAddr)

	// Set our timestamp values if they are non-zero, because unix zero is
	// different to a zero time struct.
	if resp.Timestamp != 0 {
		paymentReq.Timestamp = time.Unix(resp.Timestamp, 0)
	}

	if resp.Expiry != 0 {
		paymentReq.Expiry = time.Unix(resp.Expiry, 0)
	}

	return paymentReq, nil
}

// OpenChannel opens a channel to the peer provided with the amounts specified.
func (s *lightningClient) OpenChannel(ctx context.Context, peer route.Vertex,
	localSat, pushSat btcutil.Amount) (*wire.OutPoint, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	chanPoint, err := s.client.OpenChannelSync(
		rpcCtx, &lnrpc.OpenChannelRequest{
			NodePubkey:         peer[:],
			LocalFundingAmount: int64(localSat),
			PushSat:            int64(pushSat),
		},
	)
	if err != nil {
		return nil, err
	}

	var hash *chainhash.Hash
	switch h := chanPoint.FundingTxid.(type) {
	case *lnrpc.ChannelPoint_FundingTxidBytes:
		hash, err = chainhash.NewHash(h.FundingTxidBytes)

	case *lnrpc.ChannelPoint_FundingTxidStr:
		hash, err = chainhash.NewHashFromStr(h.FundingTxidStr)

	default:
		return nil, fmt.Errorf("unexpected outpoint type: %T",
			chanPoint.FundingTxid)
	}
	if err != nil {
		return nil, err
	}

	return &wire.OutPoint{
		Hash:  *hash,
		Index: chanPoint.OutputIndex,
	}, nil
}

// CloseChannelUpdate is an interface implemented by channel close updates.
type CloseChannelUpdate interface {
	// CloseTxid returns the closing txid of the channel.
	CloseTxid() chainhash.Hash
}

// PendingCloseUpdate indicates that our closing transaction has been broadcast.
type PendingCloseUpdate struct {
	// CloseTx is the closing transaction id.
	CloseTx chainhash.Hash
}

// CloseTxid returns the closing txid of the channel.
func (p *PendingCloseUpdate) CloseTxid() chainhash.Hash {
	return p.CloseTx
}

// ChannelClosedUpdate indicates that our channel close has confirmed on chain.
type ChannelClosedUpdate struct {
	// CloseTx is the closing transaction id.
	CloseTx chainhash.Hash
}

// CloseTxid returns the closing txid of the channel.
func (p *ChannelClosedUpdate) CloseTxid() chainhash.Hash {
	return p.CloseTx
}

// CloseChannel closes the channel provided, returning a channel that will send
// a stream of close updates, and an error channel which will receive errors if
// the channel close stream fails. This function starts a goroutine to consume
// updates from lnd, which can be cancelled by cancelling the context it was
// called with. If lnd finishes sending updates for the close (signalled by
// sending an EOF), we close the updates and error channel to signal that there
// are no more updates to be sent.
func (s *lightningClient) CloseChannel(ctx context.Context,
	channel *wire.OutPoint, force bool) (chan CloseChannelUpdate,
	chan error, error) {

	rpcCtx := s.adminMac.WithMacaroonAuth(ctx)

	stream, err := s.client.CloseChannel(rpcCtx, &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: channel.Hash[:],
			},
			OutputIndex: channel.Index,
		},
		Force: force,
	})
	if err != nil {
		return nil, nil, err
	}

	updateChan := make(chan CloseChannelUpdate)
	errChan := make(chan error)

	// sendErr is a helper which sends an error or exits because our caller
	// context was cancelled.
	sendErr := func(err error) {
		select {
		case errChan <- err:
		case <-ctx.Done():
		}
	}

	// sendUpdate is a helper which sends an update or exits because our
	// caller context was cancelled.
	sendUpdate := func(update CloseChannelUpdate) {
		select {
		case updateChan <- update:
		case <-ctx.Done():
		}
	}

	// Send updates into our channels from the stream. We will exit if the
	// server finishes sending updates, or if our context is cancelled.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for {
			// Wait to receive an update from lnd. If we receive
			// an EOF from the server, it has finished providing
			// updates so we close our update and error channels to
			// signal that it has finished sending updates. Our
			// stream will error if the caller cancels their context
			// so this call will not block us.
			resp, err := stream.Recv()
			if err == io.EOF {
				close(updateChan)
				close(errChan)
				return
			} else if err != nil {
				sendErr(err)
				return
			}

			switch update := resp.Update.(type) {
			case *lnrpc.CloseStatusUpdate_ClosePending:
				closingHash := update.ClosePending.Txid
				txid, err := chainhash.NewHash(closingHash)
				if err != nil {
					sendErr(err)
					return
				}

				closeUpdate := &PendingCloseUpdate{
					CloseTx: *txid,
				}
				sendUpdate(closeUpdate)

			case *lnrpc.CloseStatusUpdate_ChanClose:
				closingHash := update.ChanClose.ClosingTxid
				txid, err := chainhash.NewHash(closingHash)
				if err != nil {
					sendErr(err)
					return
				}

				// Create and send our update. We do not need
				// to kill our for loop here because we expect
				// the server to signal that the stream is
				// complete, which is handled above.
				closeUpdate := &ChannelClosedUpdate{
					CloseTx: *txid,
				}
				sendUpdate(closeUpdate)

			default:
				sendErr(fmt.Errorf("unknown channel close "+
					"update: %T", resp.Update))
				return
			}
		}
	}()

	return updateChan, errChan, nil
}

// Connect attempts to connect to a peer at the host specified.
func (s *lightningClient) Connect(ctx context.Context, peer route.Vertex,
	host string) error {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	_, err := s.client.ConnectPeer(rpcCtx, &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: peer.String(),
			Host:   host,
		},
	})

	return err
}
