package api

import (
	"github.com/banbox/banbot/orm"
	"github.com/banbox/banbot/web/biz"
	"github.com/banbox/banbot/web/cfg"
	"github.com/gofiber/fiber/v2"
)

func regApiKline(api fiber.Router) {
	api.Get("/symbols", getSymbols)
	api.Get("/hist", getHist)
	api.Get("/all_inds", getTaInds)
	api.Post("/calc_ind", postCalcInd)
}

func getSymbols(c *fiber.Ctx) error {
	exsList := orm.GetAllExSymbols()
	res := make([]map[string]interface{}, 0)
	for _, exs := range exsList {
		res = append(res, map[string]interface{}{
			"exchange":   exs.Exchange,
			"market":     exs.Market,
			"symbol":     exs.Symbol,
			"short_name": exs.ToShort(),
		})
	}
	return c.JSON(fiber.Map{"data": res})
}

func getHist(c *fiber.Ctx) error {
	type HistArgs struct {
		Exchange  string `query:"exchange" validate:"required"`
		Symbol    string `query:"symbol" validate:"required"`
		TimeFrame string `query:"timeframe" validate:"required"`
		FromMS    int64  `query:"from" validate:"required"`
		ToMS      int64  `query:"to" validate:"required"`
	}
	var data = new(HistArgs)
	if err := VerifyArg(c, data, ArgQuery); err != nil {
		return err
	}
	exs, err2 := orm.ParseShort(data.Exchange, data.Symbol)
	if err2 != nil {
		return err2
	}
	exchange, err2 := biz.GetExg(exs.Exchange, exs.Market, "", true)
	if err2 != nil {
		return err2
	}
	startMS, stopMS := data.FromMS, data.ToMS
	adjs, klines, err2 := orm.AutoFetchOHLCV(exchange, exs, data.TimeFrame, startMS, stopMS, 0, true, nil)
	if err2 != nil {
		return err2
	}
	return c.JSON(fiber.Map{
		"adjs": adjs,
		"data": biz.ArrKLines(klines),
	})
}

/*
getTaInds 获取云端指标列表
*/
func getTaInds(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"data": cfg.IndsCache,
	})
}

/*
postCalcInd 计算云端指标
*/
func postCalcInd(c *fiber.Ctx) error {
	type CalcArgs struct {
		Name   string      `json:"name" validate:"required"`
		Kline  [][]float64 `json:"kline" validate:"required"`
		Params []float64   `json:"params" validate:"required"`
	}
	var data = new(CalcArgs)
	if err := VerifyArg(c, data, ArgBody); err != nil {
		return err
	}
	res, err := cfg.CalcInd(data.Name, data.Kline, data.Params)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"code": 200,
		"data": res,
	})
}
