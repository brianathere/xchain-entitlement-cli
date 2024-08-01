package entitlement

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"slices"
	"sort"

	"unicode/utf8"

	"github.com/acarl005/stripansi"
	"github.com/bas-vk/xchain-entitlement-cli/entitlement/generated"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fatih/color"
	"github.com/rodaine/table"
	"github.com/spf13/cobra"
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
			tbl.AddRow(txID, color.HiRedString("NO"), "-", req.Raw.TxHash, req.Raw.BlockNumber, formatNodes(req.SelectedNodes, nil))
		} else {
			tbl.AddRow(txID, color.HiGreenString("YES"), res, req.Raw.TxHash, req.Raw.BlockNumber, formatNodes(req.SelectedNodes, res))
		}
	}

	tbl.Print()
}

func formatNodes(selectedNodes, respondedNodes []common.Address) string {
	respondedMap := make(map[common.Address]struct{})
	for _, addr := range respondedNodes {
		respondedMap[addr] = struct{}{}
	}

	var formattedNodes string
	for _, addr := range selectedNodes {
		if _, found := respondedMap[addr]; found {
			formattedNodes += color.HiGreenString(addr.Hex()) + " "
		} else {
			formattedNodes += color.HiRedString(addr.Hex()) + " "
		}
	}

	return formattedNodes
}

func fetch(
	ctx context.Context,
	client *ethclient.Client,
	cfg *Config,
) (
	[]generated.IEntitlementCheckerEntitlementCheckRequested,
	map[common.Hash][]common.Address,
) {
	var (
		from                               = cfg.BlockRange.From
		to                                 = cfg.BlockRange.To
		checkerABI, _                      = generated.IEntitlementCheckerMetaData.GetAbi()
		gatedABI, _                        = generated.IEntitlementGatedMetaData.GetAbi()
		EntitlementCheckRequestedID        = checkerABI.Events["EntitlementCheckRequested"].ID
		EntitlementCheckResultPostedID     = gatedABI.Methods["postEntitlementCheckResult"].ID
		EntitlementCheckResultPostedInputs = gatedABI.Methods["postEntitlementCheckResult"].Inputs
		requests                           []generated.IEntitlementCheckerEntitlementCheckRequested
		results                            = make(map[common.Hash][]common.Address)
		blockRangeSize                     = int64(10 * 1024)
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
			Addresses: []common.Address{cfg.BaseRegistery}, // Define the slice with the single value
			Topics: [][]common.Hash{{
				EntitlementCheckRequestedID,
			}},
		})
		if err != nil {
			panic(err)
		}

		for _, log := range logs {
			switch log.Topics[0] {
			case EntitlementCheckRequestedID:
				var req generated.IEntitlementCheckerEntitlementCheckRequested
				err := checkerABI.UnpackIntoInterface(&req, "EntitlementCheckRequested", log.Data)
				if err != nil {
					panic(err)
				}
				req.Raw = log
				sort.Slice(req.SelectedNodes, func(i, j int) bool {
					return req.SelectedNodes[i].Cmp(req.SelectedNodes[j]) < 0
				})
				requests = append(requests, req)

				reqTxId := common.BytesToHash(req.TransactionId[:])

				fmt.Printf("requested checkers %v %v\n", reqTxId, req.SelectedNodes)

				resultContract := req.ContractAddress
				start := int64(log.BlockNumber)
				end := int64(log.BlockNumber + 10) // 20 seconds

				for blockNumber := start; blockNumber <= end; blockNumber++ {
					block, err := client.BlockByNumber(ctx, big.NewInt(int64(blockNumber)))
					if err != nil {
						fmt.Printf("Failed to retrieve block: %v\n", err)
						os.Exit(1)
					}

					for _, tx := range block.Transactions() {
						if tx.To() != nil && *tx.To() == resultContract {
							txData := tx.Data()
							if len(txData) >= 4 {
								txMethodID := hex.EncodeToString(txData[:4])
								if txMethodID == hex.EncodeToString(EntitlementCheckResultPostedID) {

									from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
									if err == nil {
										parsedData, err := EntitlementCheckResultPostedInputs.Unpack(txData[4:])
										if err != nil {
											fmt.Printf("Failed to unpack data: %v\n", err)
										}

										entitlementCheckResult := parsedData[0].([32]byte)

										transactionId := common.BytesToHash(entitlementCheckResult[:])

										results[transactionId] = append(results[transactionId], from)

									} else {
										fmt.Printf("Failed to recover address: %v\n", err)
									}
								}
							}
						}
					}
				}
				fmt.Printf("responding checkers %v\n", results[reqTxId])
			}
		}
	}

	// most recent entitlement check requests first
	slices.Reverse(requests)

	return requests, results
}
