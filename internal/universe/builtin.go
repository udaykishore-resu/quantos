package universe

import (
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// The built-in universe is DEVELOPMENT FIXTURE DATA.
//
// Ticker symbols, company names, sectors and exchanges are real, because the
// simulator and the dashboard are easier to reason about with recognisable
// names. Every *numeric* attribute below (market cap, average volume, beta) is
// a synthetic order-of-magnitude placeholder chosen to give the simulator a
// plausible spread of liquidity and volatility. They are not market data, they
// are not current, and nothing in the platform presents them as such: the
// simulator's Capabilities().Simulated flag is true and every downstream
// artifact carries the research disclaimer.
//
// Point a real provider adapter and a real universe file at the platform and
// this table is never loaded.

type seed struct {
	ticker   string
	company  string
	sector   string
	industry string
	exchange domain.Exchange
	class    domain.AssetClass
	capB     float64 // synthetic market cap in $bn
	advM     float64 // synthetic average daily volume in millions of shares
	price    float64 // synthetic reference price, used for notional only
	beta     float64
}

var seeds = []seed{
	// Technology
	{"AAPL", "Apple Inc.", "Technology", "Consumer Electronics", domain.ExchangeNASDAQ, domain.AssetEquity, 3200, 55, 230, 1.15},
	{"MSFT", "Microsoft Corporation", "Technology", "Software", domain.ExchangeNASDAQ, domain.AssetEquity, 3100, 22, 420, 1.05},
	{"NVDA", "NVIDIA Corporation", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 2900, 210, 120, 1.75},
	{"AVGO", "Broadcom Inc.", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 780, 28, 165, 1.35},
	{"ORCL", "Oracle Corporation", "Technology", "Software", domain.ExchangeNYSE, domain.AssetEquity, 460, 12, 165, 1.10},
	{"CRM", "Salesforce, Inc.", "Technology", "Software", domain.ExchangeNYSE, domain.AssetEquity, 260, 8, 270, 1.25},
	{"AMD", "Advanced Micro Devices", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 240, 50, 150, 1.85},
	{"ADBE", "Adobe Inc.", "Technology", "Software", domain.ExchangeNASDAQ, domain.AssetEquity, 220, 4, 500, 1.30},
	{"INTC", "Intel Corporation", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 95, 70, 22, 1.20},
	{"QCOM", "QUALCOMM Incorporated", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 190, 9, 170, 1.30},
	{"TXN", "Texas Instruments", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 180, 6, 195, 1.00},
	{"MU", "Micron Technology", "Technology", "Semiconductors", domain.ExchangeNASDAQ, domain.AssetEquity, 115, 20, 105, 1.60},
	{"AMAT", "Applied Materials", "Technology", "Semiconductor Equipment", domain.ExchangeNASDAQ, domain.AssetEquity, 145, 8, 175, 1.55},
	{"NOW", "ServiceNow, Inc.", "Technology", "Software", domain.ExchangeNYSE, domain.AssetEquity, 190, 2, 920, 1.20},
	{"PANW", "Palo Alto Networks", "Technology", "Software", domain.ExchangeNASDAQ, domain.AssetEquity, 120, 6, 360, 1.25},
	{"SNOW", "Snowflake Inc.", "Technology", "Software", domain.ExchangeNYSE, domain.AssetEquity, 45, 5, 135, 1.70},
	{"IBM", "International Business Machines", "Technology", "IT Services", domain.ExchangeNYSE, domain.AssetEquity, 200, 5, 215, 0.85},
	{"CSCO", "Cisco Systems", "Technology", "Networking", domain.ExchangeNASDAQ, domain.AssetEquity, 200, 20, 50, 0.90},
	{"ACN", "Accenture plc", "Technology", "IT Services", domain.ExchangeNYSE, domain.AssetEquity, 210, 3, 340, 1.05},
	{"DELL", "Dell Technologies", "Technology", "Hardware", domain.ExchangeNYSE, domain.AssetEquity, 85, 8, 120, 1.40},

	// Communication Services
	{"GOOGL", "Alphabet Inc. Class A", "Communication Services", "Interactive Media", domain.ExchangeNASDAQ, domain.AssetEquity, 2100, 30, 175, 1.05},
	{"META", "Meta Platforms, Inc.", "Communication Services", "Interactive Media", domain.ExchangeNASDAQ, domain.AssetEquity, 1400, 15, 550, 1.20},
	{"NFLX", "Netflix, Inc.", "Communication Services", "Entertainment", domain.ExchangeNASDAQ, domain.AssetEquity, 300, 4, 700, 1.25},
	{"DIS", "The Walt Disney Company", "Communication Services", "Entertainment", domain.ExchangeNYSE, domain.AssetEquity, 175, 10, 95, 1.15},
	{"CMCSA", "Comcast Corporation", "Communication Services", "Media", domain.ExchangeNASDAQ, domain.AssetEquity, 155, 18, 40, 0.90},
	{"T", "AT&T Inc.", "Communication Services", "Telecom", domain.ExchangeNYSE, domain.AssetEquity, 145, 35, 20, 0.65},
	{"VZ", "Verizon Communications", "Communication Services", "Telecom", domain.ExchangeNYSE, domain.AssetEquity, 175, 20, 42, 0.55},
	{"TMUS", "T-Mobile US, Inc.", "Communication Services", "Telecom", domain.ExchangeNASDAQ, domain.AssetEquity, 250, 5, 215, 0.70},

	// Consumer Discretionary
	{"AMZN", "Amazon.com, Inc.", "Consumer Discretionary", "Internet Retail", domain.ExchangeNASDAQ, domain.AssetEquity, 1900, 40, 180, 1.25},
	{"TSLA", "Tesla, Inc.", "Consumer Discretionary", "Automobiles", domain.ExchangeNASDAQ, domain.AssetEquity, 800, 95, 250, 2.05},
	{"HD", "The Home Depot, Inc.", "Consumer Discretionary", "Home Improvement", domain.ExchangeNYSE, domain.AssetEquity, 380, 4, 385, 1.00},
	{"MCD", "McDonald's Corporation", "Consumer Discretionary", "Restaurants", domain.ExchangeNYSE, domain.AssetEquity, 210, 3, 295, 0.70},
	{"NKE", "NIKE, Inc.", "Consumer Discretionary", "Apparel", domain.ExchangeNYSE, domain.AssetEquity, 115, 9, 75, 1.10},
	{"SBUX", "Starbucks Corporation", "Consumer Discretionary", "Restaurants", domain.ExchangeNASDAQ, domain.AssetEquity, 105, 9, 92, 1.00},
	{"LOW", "Lowe's Companies", "Consumer Discretionary", "Home Improvement", domain.ExchangeNYSE, domain.AssetEquity, 140, 3, 245, 1.05},
	{"BKNG", "Booking Holdings", "Consumer Discretionary", "Travel", domain.ExchangeNASDAQ, domain.AssetEquity, 130, 0.3, 3900, 1.30},
	{"ABNB", "Airbnb, Inc.", "Consumer Discretionary", "Travel", domain.ExchangeNASDAQ, domain.AssetEquity, 80, 5, 130, 1.45},
	{"F", "Ford Motor Company", "Consumer Discretionary", "Automobiles", domain.ExchangeNYSE, domain.AssetEquity, 45, 60, 11, 1.55},
	{"GM", "General Motors Company", "Consumer Discretionary", "Automobiles", domain.ExchangeNYSE, domain.AssetEquity, 55, 18, 48, 1.45},

	// Consumer Staples
	{"WMT", "Walmart Inc.", "Consumer Staples", "Discount Stores", domain.ExchangeNYSE, domain.AssetEquity, 620, 18, 78, 0.55},
	{"COST", "Costco Wholesale", "Consumer Staples", "Discount Stores", domain.ExchangeNASDAQ, domain.AssetEquity, 400, 2, 900, 0.80},
	{"PG", "Procter & Gamble", "Consumer Staples", "Household Products", domain.ExchangeNYSE, domain.AssetEquity, 390, 7, 165, 0.45},
	{"KO", "The Coca-Cola Company", "Consumer Staples", "Beverages", domain.ExchangeNYSE, domain.AssetEquity, 280, 14, 65, 0.55},
	{"PEP", "PepsiCo, Inc.", "Consumer Staples", "Beverages", domain.ExchangeNASDAQ, domain.AssetEquity, 230, 6, 168, 0.50},
	{"PM", "Philip Morris International", "Consumer Staples", "Tobacco", domain.ExchangeNYSE, domain.AssetEquity, 190, 7, 122, 0.65},
	{"MDLZ", "Mondelez International", "Consumer Staples", "Packaged Foods", domain.ExchangeNASDAQ, domain.AssetEquity, 90, 6, 67, 0.60},

	// Health Care
	{"LLY", "Eli Lilly and Company", "Health Care", "Pharmaceuticals", domain.ExchangeNYSE, domain.AssetEquity, 800, 4, 850, 0.60},
	{"UNH", "UnitedHealth Group", "Health Care", "Managed Care", domain.ExchangeNYSE, domain.AssetEquity, 520, 4, 560, 0.70},
	{"JNJ", "Johnson & Johnson", "Health Care", "Pharmaceuticals", domain.ExchangeNYSE, domain.AssetEquity, 380, 8, 158, 0.55},
	{"ABBV", "AbbVie Inc.", "Health Care", "Biotechnology", domain.ExchangeNYSE, domain.AssetEquity, 320, 6, 180, 0.65},
	{"MRK", "Merck & Co., Inc.", "Health Care", "Pharmaceuticals", domain.ExchangeNYSE, domain.AssetEquity, 260, 9, 105, 0.50},
	{"TMO", "Thermo Fisher Scientific", "Health Care", "Life Sciences Tools", domain.ExchangeNYSE, domain.AssetEquity, 210, 2, 550, 0.95},
	{"PFE", "Pfizer Inc.", "Health Care", "Pharmaceuticals", domain.ExchangeNYSE, domain.AssetEquity, 155, 35, 27, 0.65},
	{"AMGN", "Amgen Inc.", "Health Care", "Biotechnology", domain.ExchangeNASDAQ, domain.AssetEquity, 165, 3, 305, 0.70},
	{"ISRG", "Intuitive Surgical", "Health Care", "Medical Devices", domain.ExchangeNASDAQ, domain.AssetEquity, 175, 1.5, 490, 1.15},
	{"VRTX", "Vertex Pharmaceuticals", "Health Care", "Biotechnology", domain.ExchangeNASDAQ, domain.AssetEquity, 120, 1.5, 465, 0.75},
	{"CVS", "CVS Health Corporation", "Health Care", "Healthcare Plans", domain.ExchangeNYSE, domain.AssetEquity, 70, 10, 55, 0.85},

	// Financials
	{"BRK.B", "Berkshire Hathaway Class B", "Financials", "Conglomerate", domain.ExchangeNYSE, domain.AssetEquity, 950, 4, 460, 0.85},
	{"JPM", "JPMorgan Chase & Co.", "Financials", "Banks", domain.ExchangeNYSE, domain.AssetEquity, 640, 9, 225, 1.10},
	{"V", "Visa Inc.", "Financials", "Payments", domain.ExchangeNYSE, domain.AssetEquity, 560, 6, 285, 0.95},
	{"MA", "Mastercard Incorporated", "Financials", "Payments", domain.ExchangeNYSE, domain.AssetEquity, 460, 3, 500, 1.00},
	{"BAC", "Bank of America", "Financials", "Banks", domain.ExchangeNYSE, domain.AssetEquity, 320, 35, 42, 1.30},
	{"WFC", "Wells Fargo & Company", "Financials", "Banks", domain.ExchangeNYSE, domain.AssetEquity, 210, 18, 62, 1.20},
	{"GS", "The Goldman Sachs Group", "Financials", "Investment Banking", domain.ExchangeNYSE, domain.AssetEquity, 165, 2, 520, 1.35},
	{"MS", "Morgan Stanley", "Financials", "Investment Banking", domain.ExchangeNYSE, domain.AssetEquity, 165, 8, 105, 1.30},
	{"SCHW", "The Charles Schwab Corporation", "Financials", "Brokerage", domain.ExchangeNYSE, domain.AssetEquity, 130, 9, 72, 1.20},
	{"BLK", "BlackRock, Inc.", "Financials", "Asset Management", domain.ExchangeNYSE, domain.AssetEquity, 145, 0.8, 960, 1.25},
	{"AXP", "American Express Company", "Financials", "Consumer Finance", domain.ExchangeNYSE, domain.AssetEquity, 190, 3, 265, 1.20},
	{"C", "Citigroup Inc.", "Financials", "Banks", domain.ExchangeNYSE, domain.AssetEquity, 125, 15, 65, 1.40},

	// Industrials
	{"CAT", "Caterpillar Inc.", "Industrials", "Machinery", domain.ExchangeNYSE, domain.AssetEquity, 180, 3, 370, 1.15},
	{"BA", "The Boeing Company", "Industrials", "Aerospace", domain.ExchangeNYSE, domain.AssetEquity, 110, 8, 175, 1.50},
	{"HON", "Honeywell International", "Industrials", "Conglomerate", domain.ExchangeNASDAQ, domain.AssetEquity, 135, 3, 205, 1.00},
	{"UNP", "Union Pacific Corporation", "Industrials", "Railroads", domain.ExchangeNYSE, domain.AssetEquity, 145, 3, 240, 1.05},
	{"GE", "GE Aerospace", "Industrials", "Aerospace", domain.ExchangeNYSE, domain.AssetEquity, 190, 6, 175, 1.20},
	{"LMT", "Lockheed Martin", "Industrials", "Defense", domain.ExchangeNYSE, domain.AssetEquity, 130, 1.3, 550, 0.60},
	{"DE", "Deere & Company", "Industrials", "Machinery", domain.ExchangeNYSE, domain.AssetEquity, 110, 2, 400, 1.10},
	{"UPS", "United Parcel Service", "Industrials", "Logistics", domain.ExchangeNYSE, domain.AssetEquity, 110, 4, 130, 1.05},
	{"RTX", "RTX Corporation", "Industrials", "Aerospace", domain.ExchangeNYSE, domain.AssetEquity, 155, 6, 118, 0.90},

	// Energy
	{"XOM", "Exxon Mobil Corporation", "Energy", "Integrated Oil", domain.ExchangeNYSE, domain.AssetEquity, 500, 17, 115, 0.95},
	{"CVX", "Chevron Corporation", "Energy", "Integrated Oil", domain.ExchangeNYSE, domain.AssetEquity, 280, 9, 150, 0.95},
	{"COP", "ConocoPhillips", "Energy", "Exploration & Production", domain.ExchangeNYSE, domain.AssetEquity, 125, 7, 105, 1.15},
	{"SLB", "SLB", "Energy", "Oil Services", domain.ExchangeNYSE, domain.AssetEquity, 60, 12, 42, 1.35},
	{"EOG", "EOG Resources", "Energy", "Exploration & Production", domain.ExchangeNYSE, domain.AssetEquity, 70, 4, 125, 1.20},

	// Utilities, Real Estate, Materials
	{"NEE", "NextEra Energy", "Utilities", "Electric Utilities", domain.ExchangeNYSE, domain.AssetEquity, 165, 12, 80, 0.60},
	{"DUK", "Duke Energy", "Utilities", "Electric Utilities", domain.ExchangeNYSE, domain.AssetEquity, 85, 4, 110, 0.45},
	{"SO", "The Southern Company", "Utilities", "Electric Utilities", domain.ExchangeNYSE, domain.AssetEquity, 95, 5, 88, 0.45},
	{"AMT", "American Tower Corporation", "Real Estate", "REIT", domain.ExchangeNYSE, domain.AssetEquity, 90, 3, 195, 0.85},
	{"PLD", "Prologis, Inc.", "Real Estate", "REIT", domain.ExchangeNYSE, domain.AssetEquity, 110, 4, 115, 1.10},
	{"SPG", "Simon Property Group", "Real Estate", "REIT", domain.ExchangeNYSE, domain.AssetEquity, 55, 2, 165, 1.35},
	{"LIN", "Linde plc", "Materials", "Industrial Gases", domain.ExchangeNASDAQ, domain.AssetEquity, 210, 2, 450, 0.90},
	{"SHW", "The Sherwin-Williams Company", "Materials", "Chemicals", domain.ExchangeNYSE, domain.AssetEquity, 90, 1.3, 360, 1.10},
	{"FCX", "Freeport-McMoRan", "Materials", "Copper", domain.ExchangeNYSE, domain.AssetEquity, 65, 15, 45, 1.65},
	{"NEM", "Newmont Corporation", "Materials", "Gold", domain.ExchangeNYSE, domain.AssetEquity, 55, 12, 48, 0.90},

	// Broad-market and sector ETFs, and macro references. The regime and
	// relationship engines consume these as market context, not as tradables.
	{"SPY", "SPDR S&P 500 ETF Trust", "Index", "Broad Market", domain.ExchangeARCA, domain.AssetETF, 0, 75, 560, 1.00},
	{"QQQ", "Invesco QQQ Trust", "Index", "Nasdaq 100", domain.ExchangeNASDAQ, domain.AssetETF, 0, 45, 480, 1.15},
	{"IWM", "iShares Russell 2000 ETF", "Index", "Small Cap", domain.ExchangeARCA, domain.AssetETF, 0, 30, 220, 1.20},
	{"DIA", "SPDR Dow Jones Industrial Average ETF", "Index", "Large Cap", domain.ExchangeARCA, domain.AssetETF, 0, 4, 420, 0.95},
	{"XLK", "Technology Select Sector SPDR", "Technology", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 8, 230, 1.20},
	{"XLF", "Financial Select Sector SPDR", "Financials", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 40, 46, 1.05},
	{"XLE", "Energy Select Sector SPDR", "Energy", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 18, 90, 1.10},
	{"XLV", "Health Care Select Sector SPDR", "Health Care", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 9, 150, 0.75},
	{"XLI", "Industrial Select Sector SPDR", "Industrials", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 12, 135, 1.05},
	{"XLY", "Consumer Discretionary Select Sector SPDR", "Consumer Discretionary", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 6, 200, 1.20},
	{"XLP", "Consumer Staples Select Sector SPDR", "Consumer Staples", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 12, 80, 0.55},
	{"XLU", "Utilities Select Sector SPDR", "Utilities", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 15, 75, 0.50},
	{"XLB", "Materials Select Sector SPDR", "Materials", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 6, 90, 1.05},
	{"XLRE", "Real Estate Select Sector SPDR", "Real Estate", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 6, 42, 1.00},
	{"XLC", "Communication Services Select Sector SPDR", "Communication Services", "Sector ETF", domain.ExchangeARCA, domain.AssetETF, 0, 6, 95, 1.10},
	{"VIX", "CBOE Volatility Index", "Macro", "Volatility", domain.ExchangeOther, domain.AssetIndex, 0, 0, 16, -4.0},
	{"TLT", "iShares 20+ Year Treasury Bond ETF", "Macro", "Rates", domain.ExchangeNASDAQ, domain.AssetETF, 0, 30, 92, -0.30},
	{"IEF", "iShares 7-10 Year Treasury Bond ETF", "Macro", "Rates", domain.ExchangeNASDAQ, domain.AssetETF, 0, 8, 95, -0.20},
	{"HYG", "iShares iBoxx High Yield Corporate Bond ETF", "Macro", "Credit", domain.ExchangeARCA, domain.AssetETF, 0, 30, 79, 0.45},
	{"DXY", "US Dollar Index", "Macro", "FX", domain.ExchangeOther, domain.AssetIndex, 0, 0, 103, -0.20},
	{"GLD", "SPDR Gold Shares", "Macro", "Commodity", domain.ExchangeARCA, domain.AssetETF, 0, 8, 240, 0.10},
	{"USO", "United States Oil Fund", "Macro", "Commodity", domain.ExchangeARCA, domain.AssetETF, 0, 4, 75, 0.65},
}

// Named lists derived from the seed table.
var listSP500 = []string{
	"AAPL", "MSFT", "NVDA", "AVGO", "ORCL", "CRM", "AMD", "ADBE", "INTC", "QCOM",
	"TXN", "MU", "AMAT", "NOW", "PANW", "IBM", "CSCO", "ACN", "DELL",
	"GOOGL", "META", "NFLX", "DIS", "CMCSA", "T", "VZ", "TMUS",
	"AMZN", "TSLA", "HD", "MCD", "NKE", "SBUX", "LOW", "BKNG", "ABNB", "F", "GM",
	"WMT", "COST", "PG", "KO", "PEP", "PM", "MDLZ",
	"LLY", "UNH", "JNJ", "ABBV", "MRK", "TMO", "PFE", "AMGN", "ISRG", "VRTX", "CVS",
	"BRK.B", "JPM", "V", "MA", "BAC", "WFC", "GS", "MS", "SCHW", "BLK", "AXP", "C",
	"CAT", "BA", "HON", "UNP", "GE", "LMT", "DE", "UPS", "RTX",
	"XOM", "CVX", "COP", "SLB", "EOG",
	"NEE", "DUK", "SO", "AMT", "PLD", "SPG", "LIN", "SHW", "FCX", "NEM",
}

var listNasdaq100 = []string{
	"AAPL", "MSFT", "NVDA", "AVGO", "AMD", "ADBE", "INTC", "QCOM", "TXN", "MU",
	"AMAT", "PANW", "SNOW", "CSCO", "GOOGL", "META", "NFLX", "CMCSA", "TMUS",
	"AMZN", "TSLA", "SBUX", "BKNG", "ABNB", "COST", "PEP", "MDLZ", "AMGN",
	"ISRG", "VRTX", "LIN", "DELL",
}

var listMacro = []string{"SPY", "QQQ", "IWM", "DIA", "VIX", "TLT", "IEF", "HYG", "DXY", "GLD", "USO"}

var listSectorETFs = []string{"XLK", "XLF", "XLE", "XLV", "XLI", "XLY", "XLP", "XLU", "XLB", "XLRE", "XLC"}

var listWatchlist = []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOGL", "META", "TSLA", "JPM", "XOM", "LLY"}

// Builtin returns the development universe. ListedAt is set well in the past so
// point-in-time queries behave sensibly during demos.
func Builtin() *Universe {
	u := New()
	listed := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range seeds {
		adv := s.advM * 1_000_000
		u.Add(domain.Stock{
			Ticker:         domain.NormalizeTicker(s.ticker),
			Company:        s.company,
			Sector:         s.sector,
			Industry:       s.industry,
			Exchange:       s.exchange,
			AssetClass:     s.class,
			Currency:       "USD",
			MarketCap:      s.capB * 1e9,
			SharesOut:      ternary(s.price > 0, s.capB*1e9/s.price, 0),
			AvgVolume:      adv,
			ReferencePrice: s.price,
			AvgNotional:    adv * s.price,
			Beta:           s.beta,
			ListedAt:       listed,
			Tags:           []string{"fixture"},
		})
	}
	u.SetList("sp500", toTickers(listSP500))
	u.SetList("nasdaq100", toTickers(listNasdaq100))
	u.SetList("macro", toTickers(listMacro))
	u.SetList("sector_etfs", toTickers(listSectorETFs))
	u.SetList("watchlist", toTickers(listWatchlist))
	return u
}

// MacroTickers returns the market-context symbols the regime and relationship
// engines require.
func MacroTickers() []domain.Ticker { return toTickers(listMacro) }

// SectorETFs returns the sector ETF symbols.
func SectorETFs() []domain.Ticker { return toTickers(listSectorETFs) }

func toTickers(ss []string) []domain.Ticker {
	out := make([]domain.Ticker, 0, len(ss))
	for _, s := range ss {
		out = append(out, domain.NormalizeTicker(s))
	}
	return out
}

func ternary(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}
