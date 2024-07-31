package entitlement

import (
	"context"
	"math/big"
	"slices"
	"sort"

	"github.com/acarl005/stripansi"
	"github.com/bas-vk/xchain-entitlement-cli/entitlement/generated"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fatih/color"
	"github.com/rodaine/table"
	"github.com/spf13/cobra"
	"unicode/utf8"
)

func Run(cmd *cobra.Command, args []string) {
	var (
		ctx = cmd.Context()
		cfg = config(cmd, args)
	)

	client, err := ethclient.DialContext(ctx, cfg.RPCEndpoint)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	requests, results := fetch(ctx, client, cfg)

	headerFmt := color.New(color.FgGreen, color.Underline).SprintfFunc()
	columnFmt := color.New(color.FgYellow).SprintfFunc()
	colorAwareWidthFunc := func(s string) int { return utf8.RuneCountInString(stripansi.Strip(s)) }

	tbl := table.New("Entitlement.TxId", "Processed", "Result", "Req.TransactionId", "Req.Block", "Nodes")
	tbl.WithHeaderFormatter(headerFmt).
		WithFirstColumnFormatter(columnFmt).
		WithWidthFunc(colorAwareWidthFunc)

	for _, req := range requests {
		txID := common.Hash(req.TransactionId)
		res, ok := results[txID]
		if !ok {
			tbl.AddRow(txID, color.HiRedString("NO"), "-", req.Raw.TxHash, req.Raw.BlockNumber, req.SelectedNodes)
		} else {
			tbl.AddRow(txID, color.HiGreenString("YES"), res.Result, req.Raw.TxHash, req.Raw.BlockNumber, req.SelectedNodes)
		}
	}

	tbl.Print()
}

func fetch(
	ctx context.Context,
	client *ethclient.Client,
	cfg *Config,
) (
	[]generated.IEntitlementCheckerEntitlementCheckRequested,
	map[common.Hash]generated.IEntitlementGatedEntitlementCheckResultPosted,
) {
	var (
		from                           = cfg.BlockRange.From
		to                             = cfg.BlockRange.To
		checkerABI, _                  = generated.IEntitlementCheckerMetaData.GetAbi()
		gatedABI, _                    = generated.IEntitlementGatedMetaData.GetAbi()
		EntitlementCheckRequestedID    = checkerABI.Events["EntitlementCheckRequested"].ID
		EntitlementCheckResultPostedID = gatedABI.Events["EntitlementCheckResultPosted"].ID
		requests                       []generated.IEntitlementCheckerEntitlementCheckRequested
		results                        = make(map[common.Hash]generated.IEntitlementGatedEntitlementCheckResultPosted)
		blockRangeSize                 = int64(10 * 1024)
	)

	if to == nil {
		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			panic(err)
		}
		to = head.Number
	}

	for i := from.Int64(); i < to.Int64(); i += blockRangeSize {
		logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: big.NewInt(i),
			ToBlock:   big.NewInt(min(i+blockRangeSize, to.Int64())),
			Addresses: cfg.Contracts,
			Topics: [][]common.Hash{{
				EntitlementCheckRequestedID,
				EntitlementCheckResultPostedID,
			}},
		})
		if err != nil {
			panic(err)
		}

		for _, log := range logs {
			switch log.Topics[0] {
			case EntitlementCheckRequestedID:
				var req generated.IEntitlementCheckerEntitlementCheckRequested
				err = checkerABI.UnpackIntoInterface(&req, "EntitlementCheckRequested", log.Data)
				if err != nil {
					panic(err)
				}
				req.Raw = log
				sort.Slice(req.SelectedNodes, func(i, j int) bool {
					return req.SelectedNodes[i].Cmp(req.SelectedNodes[j]) < 0
				})
				requests = append(requests, req)
			case EntitlementCheckResultPostedID:
				var res generated.IEntitlementGatedEntitlementCheckResultPosted
				err = gatedABI.UnpackIntoInterface(&res, "EntitlementCheckResultPosted", log.Data)
				if err != nil {
					panic(err)
				}
				res.TransactionId = log.Topics[1]
				res.Raw = log
				results[res.TransactionId] = res
			}
		}
	}

	// most recent entitlement check requests first
	slices.Reverse(requests)

	return requests, results
}
