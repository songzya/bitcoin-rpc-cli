package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/olivere/elastic"
	"github.com/shopspring/decimal"
	"github.com/songzya/bitcoin-rpc-cli/btcjson"
	//"github.com/dogecoinw/doged/btcjson"
)

// ROLLBACKHEIGHT 回滚个数
const ROLLBACKHEIGHT = 5

// Sync dump bitcoin chaindata to es
func (esClient *elasticClientAlias) Sync(btcClient bitcoinClientAlias) bool {
	info, err := btcClient.GetBlockChainInfo()
	if err != nil {
		sugar.Fatal("Get info error: ", err.Error())
	}
	sugar.Warn("info", info)
	btcClient.ReSetSync(info.Headers, esClient)
	return true

	var DBCurrentHeight float64
	agg, err := esClient.MaxAgg("height", "block", "block")
	if err != nil {
		if err.Error() == "query max agg error" {
			btcClient.ReSetSync(info.Headers, esClient)
			return true
		}
		sugar.Warn(strings.Join([]string{"Query max aggration error:", err.Error()}, " "))
		//return false
		DBCurrentHeight = 0
	} else {
		DBCurrentHeight = *agg
	}

	heightGap := info.Headers - int32(DBCurrentHeight)
	switch {
	case heightGap > 0:
		esClient.RollbackAndSync(DBCurrentHeight, int(ROLLBACKHEIGHT), btcClient)
	case heightGap == 0:
		esBestBlock, err := esClient.QueryEsBlockByHeight(context.TODO(), info.Headers)
		if err != nil {
			sugar.Fatal("Can't query best block in es")
		}

		nodeblock, err := btcClient.getBlock1(info.Headers)
		if err != nil {
			sugar.Fatal("Can't query block from bitcoind")
		}

		if esBestBlock.Hash != nodeblock.Hash {
			esClient.RollbackAndSync(DBCurrentHeight, int(ROLLBACKHEIGHT), btcClient)
		}
	case heightGap < 0:
		sugar.Fatal("bitcoind best height block less than max block in database , something wrong")
	}
	return true
}

func (esClient *elasticClientAlias) RollbackAndSync(from float64, size int, btcClient bitcoinClientAlias) {
	rollbackIndex := int(from) - size
	beginSynsIndex := int32(rollbackIndex)
	if rollbackIndex <= 1 {
		beginSynsIndex = 1
	}

	SyncBeginRecordIndex := strconv.FormatInt(int64(beginSynsIndex), 10)
	if beginSynsIndex != 1 {
		SyncBeginRecord, err := esClient.Get().Index("block").Type("block").Id(SyncBeginRecordIndex).Do(context.Background())
		if err != nil {
			sugar.Fatal("Query SyncBeginRecord error")
		}

		info, err := btcClient.GetBlockChainInfo()
		if err != nil {
			sugar.Fatal("Get info error: ", err.Error())
		}

		if !SyncBeginRecord.Found {
			sugar.Fatal("can't get begin block, need to be resync")
		} else {
			// 数据库倒退 5 个块再同步
			btcClient.dumpToES(beginSynsIndex, info.Headers, size, esClient)
		}
	} else {
		info, err := btcClient.GetBlockChainInfo()
		if err != nil {
			sugar.Fatal("Get info error: ", err.Error())
		}
		btcClient.dumpToES(beginSynsIndex, info.Headers, size, esClient)
	}
}

func (btcClient *bitcoinClientAlias) dumpToES(from, end int32, size int, elasticClient *elasticClientAlias) {
	sugar.Info("Get from: ", from, ",  end : ", end, ", size :", size)
	for height := from; height < end; height++ {
		dumpBlockTime := time.Now()
		//hash, err1 := btcClient.GetBlockHash(int64(2))
		//if err1 != nil {
		//	sugar.Fatal("Get block hash error: ", err1.Error())
		//} else {
		//	sugar.Info("Get block hash : ", hash)
		//}
		//info, err2 := btcClient.GetBlockChainInfo()
		//if err2 != nil {
		//	sugar.Fatal("Get block error: ", err2.Error())
		//} else {
		//	sugar.Info("Get block info: ", info)
		//}
		//cnt, err3 := btcClient.GetBlockCount()
		//if err3 != nil {
		//	sugar.Fatal("Get block count error: ", err3.Error())
		//} else {
		//	sugar.Info("Get block count: ", cnt)
		//}
		//tx, err := btcClient.getBlock1(20000)
		//if err != nil {
		//	sugar.Fatal("Get block error: ", err.Error())
		//} else {
		//	sugar.Info("Get tx info: ", tx)
		//}
		sugar.Info("Get height: ", height)
		block, err := btcClient.getBlock(height)
		if err != nil {
			sugar.Fatal("dumpToES Get block error: ", err.Error())
		} else {
			sugar.Info("Get tx info: ", block)
		}
		// 这个地址交易数据比较明显，
		// 结合 https://blockchain.info/address/12cbQLTFMXRnSzktFkuoG3eHoMeFtpTu3S 的交易数据测试验证同步逻辑 (该地址上 2009 年的交易数据)
		elasticClient.RollBackAndSyncTx(from, height, size, block)
		elasticClient.RollBackAndSyncBlock(from, height, size, block)
		sugar.Info("Dump block ", block.Height, " ", block.Hash, " dumpBlockTimeElapsed ", time.Since(dumpBlockTime))
		sugar.Fatal(" dumpBlockTimeElapsed ", time.Since(dumpBlockTime))
	}
}

func (esClient *elasticClientAlias) RollBackAndSyncTx(from, height int32, size int, block *btcjson.GetBlockVerboseTxResult) {
	// 回滚时，es 中 best height + 1 中的 vout, balance, tx 都需要回滚。
	ctx := context.Background()
	if (from != 1) && (height <= (from + int32(size+1))) {
		esClient.RollbackTxVoutBalanceByBlock(ctx, block)
	}

	esClient.syncTxVoutBalance(ctx, block)
}

func (esClient *elasticClientAlias) RollBackAndSyncBlock(from, height int32, size int, block *btcjson.GetBlockVerboseTxResult) {
	ctx := context.Background()
	if (from != 1) && (height <= (from + int32(size))) {
		_, err := esClient.Delete().Index("block").Type("block").Id(strconv.FormatInt(int64(height), 10)).Refresh("true").Do(ctx)
		if err != nil && err.Error() != "elastic: Error 404 (Not Found)" {
			sugar.Fatal("Delete block docutment error: ", err.Error())
		}
	}
	bodyParams := blockWithTxDetail(block)
	_, err := esClient.Index().Index("block").Type("block").Id(strconv.FormatInt(int64(height), 10)).BodyJson(bodyParams).Do(ctx)
	if err != nil {
		sugar.Fatal(strings.Join([]string{"Dump block docutment error", err.Error()}, " "))
	}
}

func (esClient *elasticClientAlias) syncTxVoutBalance(ctx context.Context, block *btcjson.GetBlockVerboseTxResult) {
	bulkRequest := esClient.Bulk()
	var (
		vinAddressWithAmountSlice         []Balance
		voutAddressWithAmountSlice        []Balance
		voutAddressWithAmountAndTxidSlice []AddressWithAmountAndTxid
		vinAddressWithAmountAndTxidSlice  []AddressWithAmountAndTxid
		vinAddresses                      []interface{} // All addresses related with vins in a block
		voutAddresses                     []interface{} // All addresses related with vouts in a block
		esTxs                             []*esTx
	)

	// TODO too slow, neet to optimization
	for _, tx := range block.Tx {
		var (
			voutAmount       decimal.Decimal
			vinAmount        decimal.Decimal
			txTypeVinsField  []AddressWithValueInTx
			txTypeVoutsField []AddressWithValueInTx
		)

		for _, vout := range tx.Vout {
			if result := esClient.syncVout(vout, tx, bulkRequest); result == false {
				continue
			}

			// vout amount
			voutAmount = voutAmount.Add(decimal.NewFromFloat(vout.Value))

			txTypeVoutsFieldTmp, voutAddressesTmp, voutAddressWithAmountSliceTmp, voutAddressWithAmountAndTxidSliceTmp := parseTxVout(vout, tx.Txid)
			txTypeVoutsField = append(txTypeVoutsField, txTypeVoutsFieldTmp...)
			voutAddresses = append(voutAddresses, voutAddressesTmp...) // vouts field in tx type
			voutAddressWithAmountSlice = append(voutAddressWithAmountSlice, voutAddressWithAmountSliceTmp...)
			voutAddressWithAmountAndTxidSlice = append(voutAddressWithAmountAndTxidSlice, voutAddressWithAmountAndTxidSliceTmp...)
		}

		// get es vouts with id in elasticsearch by tx vins
		indexVins := indexedVinsFun(tx.Vin)
		voutWithIDs := esClient.QueryVoutWithVinsOrVoutsUnlimitSize(ctx, indexVins)
		for _, voutWithID := range voutWithIDs {
			// vin amount
			vinAmount = vinAmount.Add(decimal.NewFromFloat(voutWithID.Vout.Value))

			esClient.updateVoutUsed(tx.Txid, voutWithID, bulkRequest)

			txTypeVinsFieldTmp, vinAddressesTmp, vinAddressWithAmountSliceTmp, vinAddressWithAmountAndTxidSliceTmp := parseESVout(voutWithID, tx.Txid)
			txTypeVinsField = append(txTypeVinsField, txTypeVinsFieldTmp...)
			vinAddresses = append(vinAddresses, vinAddressesTmp...)
			vinAddressWithAmountSlice = append(vinAddressWithAmountSlice, vinAddressWithAmountSliceTmp...)
			vinAddressWithAmountAndTxidSlice = append(vinAddressWithAmountAndTxidSlice, vinAddressWithAmountAndTxidSliceTmp...)
		}

		newESTx := esTxFun(tx, block.Hash, txTypeVinsField, txTypeVoutsField, vinAmount, voutAmount)
		esTxs = append(esTxs, newESTx)
	}

	esClient.syncVinsBalance(ctx, vinAddresses, vinAddressWithAmountSlice)

	// update(add) or insert balances related to vouts addresses
	// len(voutAddressWithSumDeposit) >= len(voutBalanceWithID)
	esClient.syncVoutsBalance(ctx, voutAddresses, voutAddressWithAmountSlice, bulkRequest)

	bulkResp, err := bulkRequest.Refresh("true").Do(ctx)
	if err != nil {
		sugar.Fatal("bulk request error: ", err.Error())
	}

	bulkResp.Created()
	bulkResp.Updated()
	bulkResp.Indexed()

	// bulk add balancejournal doc (sync vout: add balance)
	esClient.BulkInsertBalanceJournal(ctx, voutAddressWithAmountAndTxidSlice, "sync+")
	// bulk add balancejournal doc (sync vin: sub balance)
	esClient.BulkInsertBalanceJournal(ctx, vinAddressWithAmountAndTxidSlice, "sync-")
	// bulk add tx doc
	esClient.BulkInsertTxes(ctx, esTxs)
}

func (esClient *elasticClientAlias) BulkInsertBalanceJournal(ctx context.Context, balancesWithID []AddressWithAmountAndTxid, ope string) {
	p, err := esClient.BulkProcessor().Name("BulkInsertBalanceJournal").Workers(5).BulkActions(40000).Do(ctx)
	if err != nil {
		sugar.Fatal("es BulkProcessor error: BulkInsertBalanceJournal, ", err.Error())
	}

	for _, balanceID := range balancesWithID {
		newBalanceJournal := newBalanceJournalFun(balanceID.Address, ope, balanceID.Txid, balanceID.Amount)
		insertBalanceJournal := elastic.NewBulkIndexRequest().Index("balancejournal").Type("balancejournal").Doc(newBalanceJournal)
		p.Add(insertBalanceJournal)
	}
	defer p.Close()
}

func (esClient *elasticClientAlias) BulkInsertTxes(ctx context.Context, esTxs []*esTx) {
	p, err := esClient.BulkProcessor().Name("tx").Workers(5).BulkActions(40000).Do(ctx)
	if err != nil {
		sugar.Fatal("es BulkProcessor error: BulkInsertTxes, ", err.Error())
	}

	for _, tx := range esTxs {
		insertTx := elastic.NewBulkIndexRequest().Index("tx").Type("tx").Doc(tx)
		p.Add(insertTx)
	}
	defer p.Close()
}

func (esClient *elasticClientAlias) syncVoutsBalance(ctx context.Context, voutAddresses []interface{}, voutAddressWithAmountSlice []Balance, bulkRequest *elastic.BulkService) {
	// 统计区块中所有 vout 涉及到去重后的 vout 地址及其对应的增加余额
	UniqueVoutAddressesWithSumDeposit := calculateUniqueAddressWithSumForVinOrVout(voutAddresses, voutAddressWithAmountSlice)
	bulkQueryVoutBalance, err := esClient.BulkQueryBalanceUnlimitSize(ctx, voutAddresses...)
	if err != nil {
		sugar.Fatal("Query balance related with vouts address error: ", err.Error())
	}
	voutBalancesWithIDs := bulkQueryVoutBalance

	for _, voutAddressWithSumDeposit := range UniqueVoutAddressesWithSumDeposit {
		var isNewBalance bool
		isNewBalance = true
		for _, voutBalanceWithID := range voutBalancesWithIDs {
			// update balance
			if voutAddressWithSumDeposit.Address == voutBalanceWithID.Balance.Address {
				balance := voutAddressWithSumDeposit.Amount.Add(decimal.NewFromFloat(voutBalanceWithID.Balance.Amount))
				amount, _ := balance.Float64()
				updateVoutBalcne := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(voutBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkRequest.Add(updateVoutBalcne)
				isNewBalance = false
				break
			}
		}

		// if voutAddressWithSumDeposit not exist in balance ES Type, insert a docutment
		if isNewBalance {
			amount, _ := voutAddressWithSumDeposit.Amount.Float64()
			newBalance := &Balance{
				Address: voutAddressWithSumDeposit.Address,
				Amount:  amount,
			}
			//  bulk insert balance
			insertBalance := elastic.NewBulkIndexRequest().Index("balance").Type("balance").Doc(newBalance)
			bulkRequest.Add(insertBalance).Refresh("true")
		}
	}
}

func (esClient *elasticClientAlias) syncVinsBalance(ctx context.Context, vinAddresses []interface{}, vinAddressWithAmountSlice []Balance) {
	// 统计块中所有交易 vin 涉及到的地址及其对应的余额 (balance type)
	UniqueVinAddressesWithSumWithdraw := calculateUniqueAddressWithSumForVinOrVout(vinAddresses, vinAddressWithAmountSlice)
	bulkQueryVinBalance, err := esClient.BulkQueryBalanceUnlimitSize(ctx, vinAddresses...)
	if err != nil {
		sugar.Fatal("Query balance related with vin error: ", err.Error())
	}
	vinBalancesWithIDs := bulkQueryVinBalance

	// 判断去重后的区块中所有交易的 vin 涉及到的地址数量是否与从 es 数据库中查询得到的 vinBalancesWithIDs 数量是否一致
	// 不一致则说明 balance type 中存在某个地址重复数据，此时应重新同步数据 TODO
	UniqueVinAddresses := removeDuplicatesForSlice(vinAddresses...)
	if len(UniqueVinAddresses) != len(vinBalancesWithIDs) {
		sugar.Fatal("There are duplicate records in balances type")
	}

	bulkUpdateVinBalanceRequest := esClient.Bulk()
	// update(sub)  balances related to vins addresses
	// len(vinAddressWithSumWithdraw) == len(vinBalancesWithIDs)
	for _, vinAddressWithSumWithdraw := range UniqueVinAddressesWithSumWithdraw {
		for _, vinBalanceWithID := range vinBalancesWithIDs {
			if vinAddressWithSumWithdraw.Address == vinBalanceWithID.Balance.Address {
				balance := decimal.NewFromFloat(vinBalanceWithID.Balance.Amount).Sub(vinAddressWithSumWithdraw.Amount)
				amount, _ := balance.Float64()
				updateVinBalcne := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(vinBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkUpdateVinBalanceRequest.Add(updateVinBalcne).Refresh("true")
				break
			}
		}
	}
	// vin 涉及到的地址余额必须在 vout 涉及到的地址余额之前更新，原因如下：
	// 但一笔交易中的 vins 里面的地址同时出现在 vout 中（就是常见的找零），那么对于这个地址而言，必须先减去 vin 的余额，再加上 vout 的余额
	if bulkUpdateVinBalanceRequest.NumberOfActions() != 0 {
		bulkUpdateVinBalanceResp, err := bulkUpdateVinBalanceRequest.Refresh("true").Do(context.TODO())
		if err != nil {
			sugar.Fatal("update vin balance error: ", err.Error())
		}
		bulkUpdateVinBalanceResp.Updated()
	}
}

func (esClient *elasticClientAlias) syncVout(vout btcjson.Vout, tx btcjson.TxRawResult, bulkRequest *elastic.BulkService) bool {
	//  bulk insert vouts
	newVout, err := newVoutFun(vout, tx.Vin, tx.Txid)
	if err != nil {
		return false
	}
	createdVout := elastic.NewBulkIndexRequest().Index("vout").Type("vout").Doc(newVout)
	bulkRequest.Add(createdVout).Refresh("true")
	return true
}

func (esClient *elasticClientAlias) updateVoutUsed(txid string, voutWithID VoutWithID, bulkRequest *elastic.BulkService) {
	// update vout type used field
	updateVoutUsedField := elastic.NewBulkUpdateRequest().Index("vout").Type("vout").Id(voutWithID.ID).
		Doc(map[string]interface{}{"used": voutUsed{Txid: txid, VinIndex: voutWithID.Vout.Voutindex}})
	bulkRequest.Add(updateVoutUsedField).Refresh("true")
}
func (esClient *elasticClientAlias) RollbackTxVoutBalanceByBlock(ctx context.Context, block *btcjson.GetBlockVerboseTxResult) error {
	bulkRequest := esClient.Bulk()
	var (
		vinAddresses                      []interface{} // All addresses related with vins in a block
		voutAddresses                     []interface{} // All addresses related with vouts in a block
		vinAddressWithAmountSlice         []Balance
		voutAddressWithAmountSlice        []Balance
		voutAddressWithAmountAndTxidSlice []AddressWithAmountAndTxid
		vinAddressWithAmountAndTxidSlice  []AddressWithAmountAndTxid
		UniqueVinAddressesWithSumWithdraw []*AddressWithAmount // 统计区块中所有 vout 涉及到去重后的 vout 地址及其对应的增加余额
		UniqueVoutAddressesWithSumDeposit []*AddressWithAmount // 统计区块中所有 vout 涉及到去重后的 vout 地址及其对应的增加余额
		vinBalancesWithIDs                []*BalanceWithID
		voutBalancesWithIDs               []*BalanceWithID
	)

	// rollback: delete txs in es by block hash
	if e := esClient.DeleteEsTxsByBlockHash(ctx, block.Hash); e != nil {
		sugar.Info("rollback block err: ", block.Hash, " fail to delete")
	}

	for _, tx := range block.Tx {
		// es 中 vout 的 used 字段为 nil 涉及到的 vins 地址余额不用回滚

		voutWithIDSliceForVins, _ := esClient.QueryVoutsByUsedFieldAndBelongTxID(ctx, tx.Vin, tx.Txid)

		// 如果 len(voutWithIDSliceForVins) 为 0 ，则表面已经回滚过了，
		for _, voutWithID := range voutWithIDSliceForVins {
			// rollback: update vout's used to nil
			updateVoutUsedField := elastic.NewBulkUpdateRequest().Index("vout").Type("vout").Id(voutWithID.ID).
				Doc(map[string]interface{}{"used": nil})
			bulkRequest.Add(updateVoutUsedField).Refresh("true")

			_, vinAddressesTmp, vinAddressWithAmountSliceTmp, vinAddressWithAmountAndTxidSliceTmp := parseESVout(voutWithID, tx.Txid)
			vinAddresses = append(vinAddresses, vinAddressesTmp...)
			vinAddressWithAmountSlice = append(vinAddressWithAmountSlice, vinAddressWithAmountSliceTmp...)
			vinAddressWithAmountAndTxidSlice = append(vinAddressWithAmountAndTxidSlice, vinAddressWithAmountAndTxidSliceTmp...)
		}

		// get es vouts with id in elasticsearch by tx vouts
		indexVouts := indexedVoutsFun(tx.Vout, tx.Txid)
		// 没有被删除的 vouts 涉及到的 vout 地址才需要回滚余额
		voutWithIDSliceForVouts := esClient.QueryVoutWithVinsOrVoutsUnlimitSize(ctx, indexVouts)

		for _, voutWithID := range voutWithIDSliceForVouts {
			// rollback: delete vout
			deleteVout := elastic.NewBulkDeleteRequest().Index("vout").Type("vout").Id(voutWithID.ID)
			bulkRequest.Add(deleteVout).Refresh("true")

			_, voutAddressesTmp, voutAddressWithAmountSliceTmp, voutAddressWithAmountAndTxidSliceTmp := parseESVout(voutWithID, tx.Txid)
			voutAddresses = append(voutAddresses, voutAddressesTmp...)
			voutAddressWithAmountSlice = append(voutAddressWithAmountSlice, voutAddressWithAmountSliceTmp...)
			voutAddressWithAmountAndTxidSlice = append(voutAddressWithAmountAndTxidSlice, voutAddressWithAmountAndTxidSliceTmp...)
		}
	}

	// 统计块中所有交易 vin 涉及到的地址及其对应的提现余额 (balance type)
	UniqueVinAddressesWithSumWithdraw = calculateUniqueAddressWithSumForVinOrVout(vinAddresses, vinAddressWithAmountSlice)
	bulkQueryVinBalance, err := esClient.BulkQueryBalance(ctx, vinAddresses...)
	if err != nil {
		sugar.Fatal("Rollback: query vin balance error: ", err.Error())
	}
	vinBalancesWithIDs = bulkQueryVinBalance

	// 统计块中所有交易 vout 涉及到的地址及其对应的提现余额 (balance type)
	UniqueVoutAddressesWithSumDeposit = calculateUniqueAddressWithSumForVinOrVout(voutAddresses, voutAddressWithAmountSlice)
	bulkQueryVoutBalance, err := esClient.BulkQueryBalance(ctx, voutAddresses...)
	if err != nil {
		sugar.Fatal("Rollback: query vout balance error: ", err.Error())
	}
	voutBalancesWithIDs = bulkQueryVoutBalance

	// rollback: add to addresses related to vins addresses
	// 通过 vin 在 vout type 的 used 字段查出来(不为 nil)的地址余额才回滚
	bulkUpdateVinBalanceRequest := esClient.Bulk()
	// update(sub)  balances related to vins addresses
	// len(vinAddressWithSumWithdraw) == len(vinBalancesWithIDs)
	for _, vinAddressWithSumWithdraw := range UniqueVinAddressesWithSumWithdraw {
		for _, vinBalanceWithID := range vinBalancesWithIDs {
			if vinAddressWithSumWithdraw.Address == vinBalanceWithID.Balance.Address {
				balance := decimal.NewFromFloat(vinBalanceWithID.Balance.Amount).Add(vinAddressWithSumWithdraw.Amount)
				amount, _ := balance.Float64()
				updateVinBalance := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(vinBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkUpdateVinBalanceRequest.Add(updateVinBalance).Refresh("true")
				break
			}
		}
	}
	if bulkUpdateVinBalanceRequest.NumberOfActions() != 0 {
		bulkUpdateVinBalanceResp, e := bulkUpdateVinBalanceRequest.Refresh("true").Do(ctx)
		if e != nil {
			sugar.Fatal("Rollback: update vin balance error: ", err.Error())
		}
		bulkUpdateVinBalanceResp.Updated()
	}

	// update(sub) balances related to vouts addresses
	// len(voutAddressWithSumDeposit) >= len(voutBalanceWithID)
	// 没有被删除的 vouts 涉及到的 vout 地址才需要回滚余额
	for _, voutAddressWithSumDeposit := range UniqueVoutAddressesWithSumDeposit {
		for _, voutBalanceWithID := range voutBalancesWithIDs {
			if voutAddressWithSumDeposit.Address == voutBalanceWithID.Balance.Address {
				balance := decimal.NewFromFloat(voutBalanceWithID.Balance.Amount).Sub(voutAddressWithSumDeposit.Amount)
				amount, _ := balance.Float64()
				updateVinBalance := elastic.NewBulkUpdateRequest().Index("balance").Type("balance").Id(voutBalanceWithID.ID).
					Doc(map[string]interface{}{"amount": amount})
				bulkRequest.Add(updateVinBalance).Refresh("true")
				break
			}
		}
	}

	if bulkRequest.NumberOfActions() != 0 {
		bulkResp, err := bulkRequest.Refresh("true").Do(ctx)
		if err != nil {
			sugar.Fatal("Rollback: bulkRequest do error: ", err.Error())
		}
		bulkResp.Updated()
		bulkResp.Deleted()
		bulkResp.Indexed()
	}

	// bulk add balancejournal doc (rollback vout: sub balance)
	esClient.BulkInsertBalanceJournal(ctx, voutAddressWithAmountAndTxidSlice, "rollback-")
	// bulk add balancejournal doc (rollback vin: add balance)
	esClient.BulkInsertBalanceJournal(ctx, vinAddressWithAmountAndTxidSlice, "rollback+")

	return nil
}
