package entitlement

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
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

type EntitlementCheckVote struct {
	Result uint8
	RoleID *big.Int
	Count  int // keep track how many times a node has voted (should be 1, but...)
}

type EntitlementCheck struct {
	BlockNumber int64
	Request     generated.IEntitlementCheckerEntitlementCheckRequested
	Votes       map[common.Address]*EntitlementCheckVote
}

func (check *EntitlementCheck) Processed() bool {
	n := len(check.Request.SelectedNodes) / 2
	return len(check.Votes) > n
}

func (check *EntitlementCheck) Responded() string {
	var result strings.Builder
	for node := range check.Votes {
		if result.Len() > 0 {
			result.WriteString(",")
		}
		result.WriteString(node.Hex())
		result.WriteString(fmt.Sprintf(" (%d)", check.Votes[node].Count))
	}
	if result.Len() == 0 {
		return ""
	}
	return "[" + result.String() + "]"
}

func (check *EntitlementCheck) NoResponse() string {
	var result strings.Builder

	for _, node := range check.Request.SelectedNodes {
		if _, ok := check.Votes[node]; !ok {
			if result.Len() > 0 {
				result.WriteString(",")
			}
			result.WriteString(node.Hex())
		}
	}
	if result.Len() == 0 {
		return ""
	}
	return "[" + result.String() + "]"
}

func Run(cmd *cobra.Command, args []string) {
	var (
		ctx = cmd.Context()
		cfg = config(cmd, args)
	)

	requests, err := fetchEntitlementRequests(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to fetch entitlement requests: %v", err)
	}

	results, err := fetchEntitlementResults(ctx, cfg, requests)
	if err != nil {
		log.Fatalf("Failed to fetch entitlement results: %v", err)
	}

	printResults(results)
}

func fetchEntitlementRequests(ctx context.Context, cfg *Config) ([]*EntitlementCheck, error) {
	var (
		from                        = cfg.BlockRange.From
		to                          = cfg.BlockRange.To
		checkerABI, _               = generated.IEntitlementCheckerMetaData.GetAbi()
		EntitlementCheckRequestedID = checkerABI.Events["EntitlementCheckRequested"].ID
		blockRangeSize              = int64(1024)
		numWorkers                  = 32
		requestsChan                = make(chan *EntitlementCheck, 100)
		errChan                     = make(chan error, numWorkers)
		wg                          sync.WaitGroup
	)

	if to == nil {
		client, err := ethclient.DialContext(ctx, cfg.RPCEndpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to Ethereum client: %w", err)
		}
		defer client.Close()

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get latest block number: %w", err)
		}
		to = head.Number
	}

	worker := func(workerId int, blockStart, blockEnd int64) {
		log.Printf("fetchEntitlementRequests start worker %v for %v to %v", workerId, blockStart, blockEnd)
		defer wg.Done()

		client, err := ethclient.DialContext(ctx, cfg.RPCEndpoint)
		if err != nil {
			errChan <- fmt.Errorf("failed to connect to Ethereum client: %w", err)
			return
		}
		defer client.Close()

		// Process the block range in chunks of 1024 blocks
		for currentBlock := blockStart; currentBlock <= blockEnd; currentBlock += blockRangeSize {
			chunkEnd := min(currentBlock+blockRangeSize-1, blockEnd) // Calculate the end of the current chunk

			logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
				FromBlock: big.NewInt(currentBlock),
				ToBlock:   big.NewInt(chunkEnd),
				Addresses: []common.Address{cfg.BaseRegistery},
				Topics:    [][]common.Hash{{EntitlementCheckRequestedID}},
			})
			if err != nil {
				errChan <- fmt.Errorf("error fetching logs for block range %d - %d: %w", currentBlock, chunkEnd, err)
				return
			}

			// Process logs for the current chunk
			for _, log := range logs {
				if log.Topics[0] == EntitlementCheckRequestedID {
					var req generated.IEntitlementCheckerEntitlementCheckRequested
					err := checkerABI.UnpackIntoInterface(&req, "EntitlementCheckRequested", log.Data)
					if err != nil {
						errChan <- fmt.Errorf("error unpacking log data: %w", err)
						continue
					}
					req.Raw = log

					sort.Slice(req.SelectedNodes, func(i, j int) bool {
						return req.SelectedNodes[i].Cmp(req.SelectedNodes[j]) < 0
					})

					requestsChan <- &EntitlementCheck{
						BlockNumber: int64(log.BlockNumber),
						Request:     req,
						Votes:       make(map[common.Address]*EntitlementCheckVote),
					}
				}
			}

		}

		log.Printf("fetchEntitlementRequests end worker %v for block range %v to %v", workerId, blockStart, blockEnd)
	}

	// Start the workers

	totalBlocks := to.Int64() - from.Int64() + 1
	blocksPerWorker := totalBlocks / int64(numWorkers)
	extraBlocks := totalBlocks % int64(numWorkers)

	startBlock := from.Int64()

	for i := 0; i < numWorkers; i++ {
		endBlock := startBlock + blocksPerWorker - 1
		if int64(i) < extraBlocks {
			endBlock++
		}

		wg.Add(1)
		go worker(i, startBlock, endBlock)
		startBlock = endBlock + 1

	}

	go func() {
		wg.Wait()
		close(requestsChan)
		close(errChan)
	}()

	var requests []*EntitlementCheck
	for req := range requestsChan {
		requests = append(requests, req)
	}

	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	slices.Reverse(requests)
	return requests, nil
}

// EntitlementCheck and other relevant structures would be defined earlier in your code.

func fetchEntitlementResults(ctx context.Context, cfg *Config, requests []*EntitlementCheck) ([]*EntitlementCheck, error) {
	var (
		gatedABI, _                        = generated.IEntitlementGatedMetaData.GetAbi()
		EntitlementCheckResultPostedID     = gatedABI.Methods["postEntitlementCheckResult"].ID
		postFuncSelector                   = hex.EncodeToString(EntitlementCheckResultPostedID[:4])
		EntitlementCheckResultPostedInputs = gatedABI.Methods["postEntitlementCheckResult"].Inputs
		numWorkers                         = 32
		wg                                 sync.WaitGroup
		blockCache                         = make(map[int64]*types.Block)
		cacheLock                          sync.RWMutex
	)

	log.Printf("fetchEntitlementResults for %v requests", len(requests))

	// Channel to distribute work to workers
	requestChan := make(chan *EntitlementCheck)

	// Worker function
	worker := func(workerID int) {
		client, err := ethclient.DialContext(ctx, cfg.RPCEndpoint)
		if err != nil {
			log.Printf("ethclient.DialContext failed %v\n", err)
			return
		}
		defer client.Close()

		defer wg.Done()
		for req := range requestChan {
			log.Printf("Worker %d processing request %v", workerID, req.Request.TransactionId)
			resultContract := req.Request.ContractAddress

			for blockNumber := req.BlockNumber; blockNumber <= req.BlockNumber+30; blockNumber++ {
				// Check the block cache
				cacheLock.RLock()
				block, found := blockCache[blockNumber]
				cacheLock.RUnlock()

				if !found {
					// Fetch the block if it's not in the cache
					var err error
					block, err = client.BlockByNumber(ctx, big.NewInt(blockNumber))
					if err != nil {
						log.Printf("Worker %d failed to retrieve block %d: %v", workerID, blockNumber, err)
						continue
					}

					// Store the block in the cache
					cacheLock.Lock()
					blockCache[blockNumber] = block
					cacheLock.Unlock()
				}

				// Process the block transactions
				for _, tx := range block.Transactions() {
					if tx.To() != nil && *tx.To() == resultContract {
						txData := tx.Data()
						if len(txData) >= 4 {
							txFuncSelector := hex.EncodeToString(txData[:4])
							if txFuncSelector == postFuncSelector {
								from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
								if err != nil {
									log.Printf("Worker %d failed to recover address: %v", workerID, err)
									continue
								}

								parsedData, err := EntitlementCheckResultPostedInputs.Unpack(txData[4:])
								if err != nil {
									log.Printf("Worker %d failed to unpack data: %v", workerID, err)
									continue
								}

								entitlementCheckResult := parsedData[0].([32]byte)
								transactionId := common.BytesToHash(entitlementCheckResult[:])

								// Update the entitlement check votes
								if transactionId == req.Request.TransactionId {
									if vote, alreadyVoted := req.Votes[from]; alreadyVoted {
										vote.Count++
									} else {
										req.Votes[from] = &EntitlementCheckVote{
											RoleID: parsedData[1].(*big.Int),
											Result: parsedData[2].(uint8),
											Count:  1,
										}
									}
								}
							}
						}
					}
				}

				// Break out if we have all the votes
				if len(req.Request.SelectedNodes) == len(req.Votes) {
					break
				}
			}
		}
	}

	// Start 32 worker goroutines
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(i)
	}

	for _, req := range requests {
		requestChan <- req
	}

	close(requestChan)

	// Wait for all workers to finish
	wg.Wait()

	return requests, nil
}

func printResults(results []*EntitlementCheck) {
	log.Printf("printResults for %v tx", len(results))
	headerFmt := color.New(color.FgGreen, color.Underline).SprintfFunc()
	columnFmt := color.New(color.FgYellow).SprintfFunc()
	colorAwareWidthFunc := func(s string) int { return utf8.RuneCountInString(stripansi.Strip(s)) }

	tbl := table.New("Entitlement.TxId", "Processed", "Req.Transaction", "Req.Block", "Responded", "No Response")
	tbl.WithHeaderFormatter(headerFmt).
		WithFirstColumnFormatter(columnFmt).
		WithWidthFunc(colorAwareWidthFunc)

	for _, req := range results {
		processed := color.HiRedString("NO")
		if req.Processed() {
			processed = color.HiGreenString("YES")
		}

		tbl.AddRow(
			common.Hash(req.Request.TransactionId),
			processed,
			req.Request.Raw.TxHash,
			req.Request.Raw.BlockNumber,
			color.HiGreenString(req.Responded()),
			color.HiRedString(req.NoResponse()))
	}

	tbl.Print()
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
