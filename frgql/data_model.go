package frgql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/shopspring/decimal"
)

////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////
var (
	dbMutex sync.Mutex
	Db      *pgxpool.Pool
	//mulchOrderFields bimap.BiMap
)

////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////
//
func OpenDb() error {
	if Db == nil {
		dbMutex.Lock()
		defer dbMutex.Unlock()
		if Db == nil {
			cnxn, err := makeDbConnection()
			if err != nil {
				return err
			}
			Db = cnxn
		}
	}

	return nil
}

////////////////////////////////////////////////////////////////////////////
//
func CloseDb() {
	if Db != nil {
		Db.Close()
	}
}

////////////////////////////////////////////////////////////////////////////
//
func makeDbConnection() (*pgxpool.Pool, error) {

	dbId := os.Getenv("DB_ID")
	dbToken := os.Getenv("DB_TOKEN")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbCaRoot := os.Getenv("DB_CA_ROOT_PATH")

	dbName := "defaultdb"
	cluster := "pushy-iguana-1562"

	dbOptions := url.PathEscape(fmt.Sprintf("--cluster=%s", cluster))
	dbParams := fmt.Sprintf("%s?sslmode=verify-full&sslrootcert=%s&options=%s", dbName, dbCaRoot, dbOptions)
	cnxnUri := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", dbId, dbToken, dbHost, dbPort, dbParams)
	// Attempt to connect
	// log.Println("\n\nCnxn String: ", cnxnUri, "\n")
	conn, err := pgxpool.Connect(context.Background(), cnxnUri)
	if err != nil {
		return nil, err
	}
	// defer conn.Close()
	return conn, nil
}

////////////////////////////////////////////////////////////////////////////
//
type T27FrClaims struct {
	Email    string   `json:"email"`
	Roles    []string `json:"groups"`
	FullName string   `json:"name"`
	Id       string   `json:"preferred_username"`
	jwt.StandardClaims
}

func (claims *T27FrClaims) isAdmin() bool {
	for _, role := range claims.Roles {
		if strings.HasSuffix(role, "FrAdmins") {
			return true
		}
	}
	return false
}

func (claims *T27FrClaims) userId() string {
	return claims.Id
}

func (claims *T27FrClaims) doesUidMatch(uid string) bool {
	return claims.userId() == uid
}

////////////////////////////////////////////////////////////////////////////
//
func parseTokenClaimsFromCtx(ctx context.Context) (*T27FrClaims, error) {
	if v := ctx.Value("T27FrAuthorization"); v != nil {

		// Parse the token
		token, _, _ := new(jwt.Parser).ParseUnverified(v.(string), &T27FrClaims{})

		if claims, ok := token.Claims.(*T27FrClaims); ok {
			//log.Println(claims)
			return claims, nil
		}
	} else {
		return nil, errors.New("Not Authorized: Required token not found")
	}

	return nil, errors.New("Not Authorized: Invalid token")
}

////////////////////////////////////////////////////////////////////////////
//
func VerifyAdminTokenFromCtx(ctx context.Context) error {
	claims, err := parseTokenClaimsFromCtx(ctx)
	if err != nil {
		return err
	}

	if !claims.isAdmin() {
		return errors.New("Not Authorized: Not an admin user")
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////
//
func verifyUidAllowedFromCtx(ctx context.Context, uid string) error {
	claims, err := parseTokenClaimsFromCtx(ctx)
	if err != nil {
		return err
	}

	// User is admin so of course
	if claims.isAdmin() {
		return nil
	}

	if !claims.doesUidMatch(uid) {
		return errors.New(fmt.Sprintf(
			"Not Authorized: User is not admin and id does not match. Asking: %s Found: %s",
			uid, claims.userId()))
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////
//
type OwnerIdSummaryType struct {
	TotalDeliveryMinutes                int
	TotalNumBagsSold                    int
	TotalNumBagsSoldToSpread            int
	TotalAmountCollectedForDonations    string
	TotalAmountCollectedForBags         string
	TotalAmountCollectedForBagsToSpread string
	TotalAmountCollected                string
	AllocationsFromDelivery             string
	AllocationsFromBagsSold             string
	AllocationsFromBagsSpread           string
	AllocationsTotal                    string
}

////////////////////////////////////////////////////////////////////////////
//
func GetSummaryByOwnerId(ownerId string) (OwnerIdSummaryType, error) {
	log.Println("Getting Summary for onwerId: ", ownerId)

	sqlCmd := "select purchases::jsonb, amount_from_donations::string, total_amount_collected::string" +
		" from mulch_orders where order_owner_id = $1"

	rows, err := Db.Query(context.Background(), sqlCmd, ownerId)
	if err != nil {
		log.Println("User summary query failed", err)
		return OwnerIdSummaryType{}, err
	}
	defer rows.Close()

	totalCollected := decimal.NewFromInt(0)
	totalCollectedForDonations := decimal.NewFromInt(0)
	totalCollectedForBags := decimal.NewFromInt(0)
	totalCollectedForSpreading := decimal.NewFromInt(0)
	numBagsSold := 0
	numBagsToSpreadSold := 0

	for rows.Next() {
		var purchases []ProductsType
		var donationsAsStr *string
		var totalCollectedAsStr *string

		err = rows.Scan(&purchases, &donationsAsStr, &totalCollectedAsStr)
		if err != nil {
			// log.Println("Reading User Summary row failed: ", err)
			return OwnerIdSummaryType{}, err
		}
		if totalCollectedAsStr == nil {
			continue
		}
		//log.Println("TotalCollectedAsStr: ", *totalCollectedAsStr)
		total, err := decimal.NewFromString(*totalCollectedAsStr)
		if err != nil {
			return OwnerIdSummaryType{}, err
		}
		totalCollected = totalCollected.Add(total)

		if donationsAsStr != nil {
			// log.Println("DonationsStr: ", *donationsAsStr)
			donationAmt, err := decimal.NewFromString(*donationsAsStr)
			if err != nil {
				return OwnerIdSummaryType{}, err
			}
			totalCollectedForDonations = totalCollectedForDonations.Add(donationAmt)
		}

		for _, item := range purchases {
			// log.Println("ItemAmountChargedStr: ", item.AmountCharged)a
			//ISSUE #108
			item.AmountCharged = strings.Replace(item.AmountCharged, ",", "", -1)
			amt, err := decimal.NewFromString(item.AmountCharged)
			if err != nil {
				return OwnerIdSummaryType{}, err
			}
			if "bags" == item.ProductId {
				numBagsSold = numBagsSold + item.NumSold
				totalCollectedForBags = totalCollectedForBags.Add(amt)
			} else if "spreading" == item.ProductId {
				numBagsToSpreadSold = numBagsToSpreadSold + item.NumSold
				totalCollectedForSpreading = totalCollectedForSpreading.Add(amt)
			}

		}
	}

	if rows.Err() != nil {
		log.Println("Reading User Summary rows had an issue: ", err)
		return OwnerIdSummaryType{}, err
	}

	timecards, err := GetMulchTimecards(ownerId, -1, []string{"timeTotal"})
	if err != nil {
		log.Println("User summary timecard query failed", err)
		return OwnerIdSummaryType{}, err
	}
	deliveryMinutes, _ := time.ParseDuration("0s")
	for _, tc := range timecards {
		log.Println("Timecard: ", tc.TimeTotal)
		durarr := strings.Split(tc.TimeTotal, ":")
		hours, _ := time.ParseDuration(durarr[0] + "h")
		mins, _ := time.ParseDuration(durarr[1] + "m")
		secs, _ := time.ParseDuration(durarr[2] + "s")
		deliveryMinutes = deliveryMinutes + hours + mins + secs
	}

	allocationsFromDelivery := decimal.NewFromInt(0)
	allocationsFromBagsSold := decimal.NewFromInt(0)
	allocationsFromBagsSpread := decimal.NewFromInt(0)
	allocationsTotal := decimal.NewFromInt(0)
	var allocFromBagsSoldStr, allocFromBagsSpreadStr, allocFromDeliveryStr, allocTotalStr *string

	sqlCmd = "select allocation_from_bags_sold::string, allocation_from_bags_spread::string, " +
		"allocation_from_delivery::string, allocation_total::string from allocation_summary where allocation_summary.uid=$1"
	log.Println("SqlCmd: ", sqlCmd)
	err = Db.QueryRow(context.Background(), sqlCmd, ownerId).Scan(&allocFromBagsSoldStr, &allocFromBagsSpreadStr, &allocFromDeliveryStr, &allocTotalStr)
	if err == nil {
		log.Println("Allocation summary query for: ", ownerId, "alloc: ", allocFromBagsSoldStr)
		if nil != allocFromBagsSoldStr {
			allocationsFromBagsSold, err = decimal.NewFromString(*allocFromBagsSoldStr)
			if err != nil {
				return OwnerIdSummaryType{}, err
			}
		}

		if nil != allocFromBagsSpreadStr {
			allocationsFromBagsSpread, err = decimal.NewFromString(*allocFromBagsSpreadStr)
			if err != nil {
				return OwnerIdSummaryType{}, err
			}
		}
		if nil != allocFromDeliveryStr {
			allocationsFromDelivery, err = decimal.NewFromString(*allocFromDeliveryStr)
			if err != nil {
				return OwnerIdSummaryType{}, err
			}
		}

		allocationsTotal, err = decimal.NewFromString(*allocTotalStr)
		if err != nil {
			return OwnerIdSummaryType{}, err
		}
	} else {
		log.Println("Allocation summary query for: ", ownerId, "failed: ", err)
	}

	return OwnerIdSummaryType{
		TotalDeliveryMinutes:                int(math.Floor(deliveryMinutes.Minutes())),
		TotalNumBagsSold:                    numBagsSold,
		TotalNumBagsSoldToSpread:            numBagsToSpreadSold,
		TotalAmountCollectedForDonations:    totalCollectedForDonations.StringFixedBank(4),
		TotalAmountCollectedForBags:         totalCollectedForBags.StringFixedBank(4),
		TotalAmountCollectedForBagsToSpread: totalCollectedForSpreading.StringFixedBank(4),
		TotalAmountCollected:                totalCollected.StringFixedBank(4),
		AllocationsFromDelivery:             allocationsFromDelivery.StringFixedBank(4),
		AllocationsFromBagsSold:             allocationsFromBagsSold.StringFixedBank(4),
		AllocationsFromBagsSpread:           allocationsFromBagsSpread.StringFixedBank(4),
		AllocationsTotal:                    allocationsTotal.StringFixedBank(4),
	}, nil
}

////////////////////////////////////////////////////////////////////////////
//
type TopSellerType struct {
	Name                 string
	TotalAmountCollected string
}

////////////////////////////////////////////////////////////////////////////
//
type GroupSummaryType struct {
	GroupId              string
	TotalAmountCollected string
}

////////////////////////////////////////////////////////////////////////////
//
type TroopSummaryType struct {
	TotalAmountCollected string
	GroupSummary         []GroupSummaryType
	TopSellers           []TopSellerType
}

////////////////////////////////////////////////////////////////////////////
//
func GetTroopSummary(numTopSellers int) (TroopSummaryType, error) {
	log.Println("Getting Troop Summary with this many top sellers: ", numTopSellers)

	sqlCmd := "select users.name, users.group_id, sum(total_amount_collected)::string from mulch_orders" +
		" inner join users on (mulch_orders.order_owner_id = users.id) where" +
		" total_amount_collected is not null group by order_owner_id, users.name, users.group_id"

	rows, err := Db.Query(context.Background(), sqlCmd)
	if err != nil {
		log.Println("Troop summary query failed", err)
		return TroopSummaryType{}, err
	}
	defer rows.Close()

	troopTotal := decimal.NewFromInt(0)
	groupTotals := make(map[string]decimal.Decimal)
	topSellers := []TopSellerType{}

	for rows.Next() {
		var name string
		var group string
		var totalAsStr string

		err = rows.Scan(&name, &group, &totalAsStr)
		if err != nil {
			log.Println("Reading Summary row failed: ", err)
			return TroopSummaryType{}, err
		}
		total, err := decimal.NewFromString(totalAsStr)
		if err != nil {
			return TroopSummaryType{}, err
		}
		troopTotal = troopTotal.Add(total)
		group_val, is_present := groupTotals[group]
		if is_present {
			groupTotals[group] = group_val.Add(total)
		} else {
			groupTotals[group] = total
		}

		topSellers = append(topSellers, TopSellerType{Name: name, TotalAmountCollected: totalAsStr})
	}

	if rows.Err() != nil {
		log.Println("Reading Summary rows had an issue: ", err)
		return TroopSummaryType{}, err
	}
	groupSummary := []GroupSummaryType{}
	for k, v := range groupTotals {
		groupSummary = append(groupSummary, GroupSummaryType{GroupId: k, TotalAmountCollected: v.String()})
	}

	sort.SliceStable(topSellers, func(r, l int) bool {
		//I thought about options since total was parsed above but ulitmately felt like this was more memory
		// efficient if not processor efficient
		r_total, err := decimal.NewFromString(topSellers[r].TotalAmountCollected)
		if err != nil {
			return false
		}
		l_total, err := decimal.NewFromString(topSellers[l].TotalAmountCollected)
		if err != nil {
			return false
		}
		return r_total.GreaterThan(l_total)
	})
	if len(topSellers) > numTopSellers {
		topSellers = topSellers[0:numTopSellers]
	}

	return TroopSummaryType{
		TotalAmountCollected: troopTotal.String(),
		GroupSummary:         groupSummary,
		TopSellers:           topSellers,
	}, nil

}

type NeighborhoodSummaryType struct {
	Neighborhood string `json:"neighborhood"`
	NumOrders    int    `json:"numOrders"`
}

////////////////////////////////////////////////////////////////////////////
//
func GetNeighborhoodSummary() ([]NeighborhoodSummaryType, error) {
	log.Println("Getting Neighborhood Summary")

	sqlCmd := "select customer_neighborhood, count(*) from mulch_orders group by customer_neighborhood"

	rows, err := Db.Query(context.Background(), sqlCmd)
	if err != nil {
		log.Println("Neighborhood summary query failed", err)
		return nil, err
	}
	defer rows.Close()

	results := []NeighborhoodSummaryType{}

	for rows.Next() {
		result := NeighborhoodSummaryType{}

		err = rows.Scan(&result.Neighborhood, &result.NumOrders)
		if err != nil {
			log.Println("Reading Summary row failed: ", err)
			return nil, err
		}

		results = append(results, result)
	}

	if rows.Err() != nil {
		log.Println("Reading Summary rows had an issue: ", err)
		return nil, err
	}
	return results, nil
}

////////////////////////////////////////////////////////////////////////////
//
type CustomerType struct {
	Name         string
	Addr1        string
	Addr2        *string
	Phone        string
	Email        *string
	Neighborhood string
}

////////////////////////////////////////////////////////////////////////////
//
type ProductsType struct {
	ProductId     string `json:"productId"`
	NumSold       int    `json:"numSold"`
	AmountCharged string `json:"amountCharged,omitempty"`
}

////////////////////////////////////////////////////////////////////////////
//
type MulchOrderType struct {
	OrderId                   string
	OwnerId                   string
	LastModifiedTime          string
	SpecialInstructions       *string
	AmountFromDonations       *string
	AmountFromPurchases       *string
	AmountFromCashCollected   *string
	AmountFromChecksCollected *string
	AmountTotalCollected      *string
	CheckNumbers              *string
	WillCollectMoneyLater     *bool
	IsVerified                *bool
	Spreaders                 []string
	Customer                  CustomerType
	Purchases                 []ProductsType
	DeliveryId                *int   // Not in archived GraphQL
	YearOrdered               string // Not in non archived GraphQL
}

////////////////////////////////////////////////////////////////////////////
//
type GetMulchOrdersParams struct {
	OwnerId       string
	GqlFields     []string
	IsFromArchive bool
	ArchiveYear   string
}

////////////////////////////////////////////////////////////////////////////
//
func mulchOrderGql2SqlMap(gqlFields []string, orderOutput *MulchOrderType) ([]string, []interface{}, string) {

	sqlFields := []string{}
	inputs := []interface{}{}
	joinSql := ""
	for _, gqlField := range gqlFields {
		// log.Println(gqlField)
		switch {
		case gqlField == "orderId":
			inputs = append(inputs, &orderOutput.OrderId)
			sqlFields = append(sqlFields, "mulch_orders.order_id")
		case gqlField == "ownerId":
			inputs = append(inputs, &orderOutput.OwnerId)
			sqlFields = append(sqlFields, "order_owner_id")
		case gqlField == "amountTotalCollected":
			inputs = append(inputs, &orderOutput.AmountTotalCollected)
			sqlFields = append(sqlFields, "total_amount_collected::string")
		case gqlField == "yearOrdered":
			inputs = append(inputs, &orderOutput.YearOrdered)
			sqlFields = append(sqlFields, "year_ordered::string")
		case gqlField == "purchases":
			inputs = append(inputs, &orderOutput.Purchases)
			sqlFields = append(sqlFields, "purchases::jsonb")
		case gqlField == "last_modified_time":
			inputs = append(inputs, &orderOutput.LastModifiedTime)
			sqlFields = append(sqlFields, "last_modified_time")
		case gqlField == "specialInstructions":
			inputs = append(inputs, &orderOutput.SpecialInstructions)
			sqlFields = append(sqlFields, "special_instructions")
		case gqlField == "amountFromDonations":
			inputs = append(inputs, &orderOutput.AmountFromDonations)
			sqlFields = append(sqlFields, "amount_from_donations::string")
		case gqlField == "amountFromPurchases":
			inputs = append(inputs, &orderOutput.AmountFromPurchases)
			sqlFields = append(sqlFields, "amount_from_purchases::string")
		case gqlField == "amountFromCashCollected":
			inputs = append(inputs, &orderOutput.AmountFromCashCollected)
			sqlFields = append(sqlFields, "cash_amount_collected::string")
		case gqlField == "amountFromChecksCollected":
			inputs = append(inputs, &orderOutput.AmountFromChecksCollected)
			sqlFields = append(sqlFields, "check_amount_collected::string")
		case gqlField == "checkNumbers":
			inputs = append(inputs, &orderOutput.CheckNumbers)
			sqlFields = append(sqlFields, "check_numbers::string")
		case gqlField == "deliveryId":
			inputs = append(inputs, &orderOutput.DeliveryId)
			sqlFields = append(sqlFields, "delivery_id")
		case gqlField == "willCollectMoneyLater":
			inputs = append(inputs, &orderOutput.WillCollectMoneyLater)
			sqlFields = append(sqlFields, "will_collect_money_later")
		case gqlField == "isVerified":
			inputs = append(inputs, &orderOutput.IsVerified)
			sqlFields = append(sqlFields, "is_verified")
		case gqlField == "spreaders":
			inputs = append(inputs, &orderOutput.Spreaders)
			sqlFields = append(sqlFields, "spreaders::jsonb")
			joinSql = "LEFT JOIN mulch_spreaders ON mulch_orders.order_id = mulch_spreaders.order_id"
		case gqlField == "customer":
			inputs = append(inputs, &orderOutput.Customer.Name)
			sqlFields = append(sqlFields, "customer_name")
			inputs = append(inputs, &orderOutput.Customer.Addr1)
			sqlFields = append(sqlFields, "customer_addr1")
			inputs = append(inputs, &orderOutput.Customer.Addr2)
			sqlFields = append(sqlFields, "customer_addr2")
			inputs = append(inputs, &orderOutput.Customer.Phone)
			sqlFields = append(sqlFields, "customer_phone")
			inputs = append(inputs, &orderOutput.Customer.Email)
			sqlFields = append(sqlFields, "customer_email")
			inputs = append(inputs, &orderOutput.Customer.Neighborhood)
			sqlFields = append(sqlFields, "customer_neighborhood")
		default:
			log.Println("Do not know how to handle GraphQL Field: ", gqlField)
		}

	}
	return sqlFields, inputs, joinSql
}

////////////////////////////////////////////////////////////////////////////
//
func GetMulchOrders(params GetMulchOrdersParams) []MulchOrderType {

	//select order_owner_id, spreaders from mulch_orders left join mulch_spreaders on mulch_orders.order_id = mulch_spreaders.order_id
	//where mulch_orders.order_id = '2a166081-787f-4ff6-9477-31b21b6ca2f7';

	order := MulchOrderType{}
	sqlFields, _, joinSql := mulchOrderGql2SqlMap(params.GqlFields, &order)

	dbTable := "mulch_orders"
	if params.IsFromArchive {
		dbTable = "archived_mulch_orders"
	}

	if 0 == len(params.OwnerId) {
		log.Println("Retrieving mulch orders. ", "Is targeting archive: ", params.IsFromArchive)

	} else {
		log.Println("Retrieving mulch orders. ", "Is targeting archive: ", params.IsFromArchive, " OwnerId: ", params.OwnerId)

	}

	doQuery := func(id *string, dbTable *string, sqlFields []string) (pgx.Rows, error) {
		sqlCmd := fmt.Sprintf("select %s from %s %s", strings.Join(sqlFields, ","), *dbTable, joinSql)
		if len(*id) == 0 {
			log.Println("SqlCmd: ", sqlCmd)
			return Db.Query(context.Background(), sqlCmd)
		} else {
			sqlCmd = sqlCmd + " where order_owner_id=$1"
			log.Println("SqlCmd: ", sqlCmd)
			return Db.Query(context.Background(), sqlCmd, *id)
		}
	}

	orders := []MulchOrderType{}
	rows, err := doQuery(&params.OwnerId, &dbTable, sqlFields)
	if err != nil {
		log.Println("Mulch Orders query failed", err)
		return orders
	}
	defer rows.Close()

	for rows.Next() {
		order := MulchOrderType{}
		_, inputs, _ := mulchOrderGql2SqlMap(params.GqlFields, &order)
		err = rows.Scan(inputs...)
		if err != nil {
			log.Println("Reading mulch order row failed: ", err)
			continue
		}
		orders = append(orders, order)
	}

	if rows.Err() != nil {
		log.Println("Reading mulch order rows had an issue: ", err)
		return []MulchOrderType{}
	}
	return orders
}

////////////////////////////////////////////////////////////////////////////
//
type GetMulchOrderParams struct {
	OrderId       string
	GqlFields     []string
	IsFromArchive bool
}

////////////////////////////////////////////////////////////////////////////
//
func GetMulchOrder(params GetMulchOrderParams) MulchOrderType {
	log.Println("Retrieving mulch order. ", "Is targeting archive: ", params.IsFromArchive, " OrderId: ", params.OrderId)

	order := MulchOrderType{}
	sqlFields, inputs, joinSql := mulchOrderGql2SqlMap(params.GqlFields, &order)

	dbTable := "mulch_orders"
	if params.IsFromArchive {
		dbTable = "archived_mulch_orders"
	}
	sqlCmd := fmt.Sprintf("select %s from %s %s where mulch_orders.order_id=$1", strings.Join(sqlFields, ","), dbTable, joinSql)
	log.Println("SqlCmd: ", sqlCmd)
	err := Db.QueryRow(context.Background(), sqlCmd, params.OrderId).Scan(inputs...)
	if err != nil {
		log.Println("Mulch order query for: ", params.OrderId, " failed", err)
	}
	// log.Println("Purchases: ", order.Purchases)
	return order
}

func OrderType2Sql(order MulchOrderType) ([]string, []string, []interface{}) {
	order.LastModifiedTime = time.Now().UTC().Format(time.RFC3339)
	values := []interface{}{}
	valIdxs := []string{}
	valIdx := 1
	sqlFields := []string{}

	// Do OrderID first because it is always there
	sqlFields = append(sqlFields, "order_id")
	values = append(values, order.OrderId)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::uuid", valIdx))
	valIdx++

	sqlFields = append(sqlFields, "last_modified_time")
	values = append(values, order.LastModifiedTime)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::timestamp", valIdx))
	valIdx++

	if len(order.OwnerId) != 0 {
		sqlFields = append(sqlFields, "order_owner_id")
		values = append(values, order.OwnerId)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(order.Purchases) != 0 {
		sqlFields = append(sqlFields, "purchases")
		values = append(values, order.Purchases)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::jsonb", valIdx))
		valIdx++
	}
	if nil != order.SpecialInstructions {
		sqlFields = append(sqlFields, "special_instructions")
		values = append(values, *order.SpecialInstructions)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != order.AmountFromDonations {
		sqlFields = append(sqlFields, "amount_from_donations")
		values = append(values, *order.AmountFromDonations)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}
	if nil != order.AmountFromPurchases {
		sqlFields = append(sqlFields, "amount_from_purchases")
		values = append(values, *order.AmountFromPurchases)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}
	if nil != order.AmountFromCashCollected {
		sqlFields = append(sqlFields, "cash_amount_collected")
		values = append(values, *order.AmountFromCashCollected)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}
	if nil != order.AmountFromChecksCollected {
		sqlFields = append(sqlFields, "check_amount_collected")
		values = append(values, *order.AmountFromChecksCollected)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}
	if nil != order.AmountTotalCollected {
		sqlFields = append(sqlFields, "total_amount_collected")
		values = append(values, *order.AmountTotalCollected)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}
	if nil != order.CheckNumbers {
		sqlFields = append(sqlFields, "check_numbers")
		values = append(values, *order.CheckNumbers)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != order.DeliveryId {
		sqlFields = append(sqlFields, "delivery_id")
		values = append(values, *order.DeliveryId)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::int", valIdx))
		valIdx++
	}
	if nil != order.WillCollectMoneyLater {
		sqlFields = append(sqlFields, "will_collect_money_later")
		values = append(values, *order.WillCollectMoneyLater)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::bool", valIdx))
		valIdx++
	}
	if nil != order.IsVerified {
		sqlFields = append(sqlFields, "is_verified")
		values = append(values, *order.IsVerified)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::bool", valIdx))
		valIdx++
	}
	if len(order.Customer.Name) != 0 {
		sqlFields = append(sqlFields, "customer_name")
		values = append(values, order.Customer.Name)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(order.Customer.Addr1) != 0 {
		sqlFields = append(sqlFields, "customer_addr1")
		values = append(values, order.Customer.Addr1)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != order.Customer.Addr2 {
		sqlFields = append(sqlFields, "customer_addr2")
		values = append(values, *order.Customer.Addr2)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(order.Customer.Phone) != 0 {
		sqlFields = append(sqlFields, "customer_phone")
		values = append(values, order.Customer.Phone)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != order.Customer.Email {
		sqlFields = append(sqlFields, "customer_email")
		values = append(values, *order.Customer.Email)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(order.Customer.Neighborhood) != 0 {
		sqlFields = append(sqlFields, "customer_neighborhood")
		values = append(values, order.Customer.Neighborhood)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}

	return sqlFields, valIdxs, values
}

////////////////////////////////////////////////////////////////////////////
//
func CreateMulchOrder(ctx context.Context, order MulchOrderType) (string, error) {
	log.Println("Creating Order: ", order)

	if 0 == len(order.OrderId) {
		return "", errors.New("orderId must be provided for a new record")
	}
	if 0 == len(order.OwnerId) {
		return "", errors.New("ownerId must be provided for a new record")
	}

	if err := verifyUidAllowedFromCtx(ctx, order.OwnerId); err != nil {
		return "", err
	}

	if 0 == len(order.Customer.Neighborhood) || "none" == order.Customer.Neighborhood {
		return "", errors.New("Neighborhood must be provided for a new record")
	}
	if 0 == len(order.Customer.Name) {
		return "", errors.New("Name must be provided for a new record")
	}
	if 0 == len(order.Customer.Addr1) {
		return "", errors.New("Address 1 must be provided for a new record")
	}
	if 0 == len(order.Customer.Phone) {
		return "", errors.New("Phone must be provided for a new record")
	}
	if 0 == len(*order.AmountTotalCollected) {
		return "", errors.New("Order purchases are empty and must be provided for a new record")
	}
	sqlFields, valIdxs, values := OrderType2Sql(order)

	sqlCmd := fmt.Sprintf("insert into mulch_orders(%s) values (%s)",
		strings.Join(sqlFields, ","), strings.Join(valIdxs, ","))

	log.Println("Creating Order sqlCmd: ", sqlCmd)
	_, err := Db.Exec(context.Background(), sqlCmd, values...)
	if err != nil {
		return "", err
	}

	return order.OrderId, nil
}

////////////////////////////////////////////////////////////////////////////
//
func UpdateMulchOrder(ctx context.Context, order MulchOrderType) (bool, error) {
	log.Println("Updating Order: ", order)

	if 0 == len(order.OrderId) {
		return false, errors.New("orderId must be provided for updated record")
	}
	if 0 == len(order.OwnerId) {
		return false, errors.New("ownerId must be provided for updated record")
	}

	if err := verifyUidAllowedFromCtx(ctx, order.OwnerId); err != nil {
		return false, err
	}

	//This was actually only updating the specified fields not updating the optional ones so changing to
	// delete existing record and adding new one
	/*
			sqlFields, valIdxs, values := OrderType2Sql(order)

			updateSqlFields := []string{}
			for i, sqlField := range sqlFields {
				updateSqlFields = append(updateSqlFields, fmt.Sprintf("%s = %s", sqlField, valIdxs[i]))
			}

			updateSqlFields = updateSqlFields[1:] //Pop off Order id from the list
			//values still has OrderId at pos 0 which is what we want so don't need to chop it off

			sqlCmd := fmt.Sprintf("update mulch_orders set %s where order_id = $1", strings.Join(updateSqlFields, ","))

			log.Println("Updating Order sqlCmd: ", sqlCmd)
			res, err := Db.Exec(context.Background(), sqlCmd, values...)
			if err != nil {
				return false, err
			}
			if 1 != res.RowsAffected() {
				return false, errors.New("There were 0 records updated")
			}

		        return true, nil
	*/

	sqlFields, valIdxs, values := OrderType2Sql(order)

	sqlCmd := fmt.Sprintf("insert into mulch_orders(%s) values (%s)",
		strings.Join(sqlFields, ","), strings.Join(valIdxs, ","))

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	log.Println("Deleting order for update: ", order.OrderId)
	_, err = trxn.Exec(context.Background(), "delete from mulch_orders where order_id = $1", order.OrderId)
	if err != nil {
		trxn.Rollback(context.Background())
		log.Println("Failed to delete order for updating: ", order.OrderId, " failed because: ", err)
		return false, err
	}
	log.Println("Updating(by inserting) Order sqlCmd: ", sqlCmd)
	_, err = trxn.Exec(context.Background(), sqlCmd, values...)
	if err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}
	return true, nil

}

////////////////////////////////////////////////////////////////////////////
//
func DeleteMulchOrder(ctx context.Context, orderId string) (bool, error) {
	log.Println("Deleteing Order with order id: ", orderId)

	// Because we want to validate that the order owner or admin are the only 2 people that can delete
	//  we have to pull the order up first to get the original order id
	sqlCmd := "select order_owner_id::string from mulch_orders where mulch_orders.order_id=$1"
	log.Println("SqlCmd: ", sqlCmd)

	var orderOwner string
	if err := Db.QueryRow(context.Background(), sqlCmd, orderId).Scan(&orderOwner); err != nil {
		log.Println("Delete Mulch order query for: ", orderId, " failed because:", err)
		return false, err
	}

	if err := verifyUidAllowedFromCtx(ctx, orderOwner); err != nil {
		return false, err
	}

	_, err := Db.Exec(context.Background(), "delete from mulch_orders where order_id=$1", orderId)
	if err != nil {
		return false, err
	}
	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type MulchDeliveryConfigType struct {
	Id                 int    `json:"id"`
	Date               string `json:"date"`
	NewOrderCutoffDate string `json:"newOrderCutoffDate"`
}

////////////////////////////////////////////////////////////////////////////
//
type ProductPriceBreaks struct {
	Gt        int    `json:"gt"`
	UnitPrice string `json:"unitPrice"`
}

////////////////////////////////////////////////////////////////////////////
//
type ProductType struct {
	Id          string               `json:"id"`
	Label       string               `json:"label"`
	MinUnits    int                  `json:"minUnits"`
	UnitPrice   string               `json:"unitPrice"`
	PriceBreaks []ProductPriceBreaks `json:"priceBreaks"`
}

////////////////////////////////////////////////////////////////////////////
//
type FinalizationDataType struct {
	BankDeposited              string `json:"bankDeposited"`
	MulchCost                  string `json:"mulchCost"`
	PerBagCost                 string `json:"perBagCost"`
	ProfitsFromBags            string `json:"profitsFromBags"`
	MulchSalesGross            string `json:"mulchSalesGross"`
	MoneyPoolForTroop          string `json:"moneyPoolForTroop"`
	MoneyPoolForScoutsSubPools string `json:"moneyPoolForScoutsSubPools"`
	MoneyPoolForScoutsSales    string `json:"moneyPoolForScoutsSales"`
	MoneyPoolForScoutsDelivery string `json:"moneyPoolForScoutsDelivery"`
	PerBagAvgEarnings          string `json:"perBagAvgEarnings"`
	DeliveryEarningsPerMinute  string `json:"deliveryEarningsPerMinute"`
}

////////////////////////////////////////////////////////////////////////////
//
type FrConfigType struct {
	Kind                 string                     `json:"kind"`
	Description          string                     `json:"description"`
	LastModifiedTime     string                     `json:"lastModifiedTime"`
	IsLocked             *bool                      `json:"isLocked"`
	MulchDeliveryConfigs *[]MulchDeliveryConfigType `json:"mulchDeliveryConfigs"`
	Products             []ProductType              `json:"products"`
	FinalizationData     *FinalizationDataType      `json:"finalizationData"`
}

////////////////////////////////////////////////////////////////////////////
//
func GetFundraiserConfig(gqlFields []string) (FrConfigType, error) {

	log.Println("Retrieving Fundraiser Config")

	frConfig := FrConfigType{}
	params := []interface{}{}
	sqlFields := []string{}

	for _, gqlField := range gqlFields {
		switch {
		case "kind" == gqlField:
			params = append(params, &frConfig.Kind)
			sqlFields = append(sqlFields, "kind")
		case "description" == gqlField:
			params = append(params, &frConfig.Description)
			sqlFields = append(sqlFields, "description")
		case "lastModifiedTime" == gqlField:
			params = append(params, &frConfig.LastModifiedTime)
			sqlFields = append(sqlFields, "last_modified_time::string")
		case "mulchDeliveryConfigs" == gqlField:
			params = append(params, &frConfig.MulchDeliveryConfigs)
			sqlFields = append(sqlFields, "mulch_delivery_configs::jsonb")
		case "products" == gqlField:
			params = append(params, &frConfig.Products)
			sqlFields = append(sqlFields, "products::jsonb")
		case "finalizationData" == gqlField:
			params = append(params, &frConfig.FinalizationData)
			sqlFields = append(sqlFields, "finalization_data::jsonb")
		case "isLocked" == gqlField:
			params = append(params, &frConfig.IsLocked)
			sqlFields = append(sqlFields, "is_locked")
		case "users" == gqlField:
		case "neighborhoods" == gqlField:
			//Skipping because it is handled seperately
		default:
			return frConfig, errors.New(fmt.Sprintf("Unknown fundraiser config field: %s", gqlField))
		}

	}

	sqlCmd := fmt.Sprintf("select %s from fundraiser_config", strings.Join(sqlFields, ","))
	log.Println("SqlCmd: ", sqlCmd)
	err := Db.QueryRow(context.Background(), sqlCmd).Scan(params...)
	if err != nil {
		log.Println("Fundraiser config query failed", err)
		return FrConfigType{}, err
	}
	return frConfig, nil
}

////////////////////////////////////////////////////////////////////////////
//
func FrConfigType2Sql(frConfig FrConfigType) ([]string, []string, []interface{}) {
	values := []interface{}{}
	sqlFields := []string{}
	valIdxs := []string{}
	valIdx := 1
	if len(frConfig.Kind) != 0 {
		sqlFields = append(sqlFields, "kind")
		values = append(values, frConfig.Kind)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(frConfig.Description) != 0 {
		sqlFields = append(sqlFields, "description")
		values = append(values, frConfig.Description)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if len(frConfig.Products) != 0 {
		sqlFields = append(sqlFields, "products")
		values = append(values, frConfig.Products)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::jsonb", valIdx))
		valIdx++
	}
	if nil != frConfig.MulchDeliveryConfigs {
		sqlFields = append(sqlFields, "mulch_delivery_configs")
		values = append(values, *frConfig.MulchDeliveryConfigs)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::jsonb", valIdx))
		valIdx++
	}
	if nil != frConfig.FinalizationData {
		sqlFields = append(sqlFields, "finalization_data")
		values = append(values, *frConfig.FinalizationData)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::jsonb", valIdx))
		valIdx++
	}
	if nil != frConfig.IsLocked {
		// Unfortunately hard to detect if this is set or not
		sqlFields = append(sqlFields, "is_locked")
		values = append(values, *frConfig.IsLocked)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::bool", valIdx))
		valIdx++
	}

	// Always do timestamp
	sqlFields = append(sqlFields, "last_modified_time")
	values = append(values, frConfig.LastModifiedTime)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::timestamp", valIdx))
	return sqlFields, valIdxs, values
}

////////////////////////////////////////////////////////////////////////////
//
func setFundraiserConfigWithTrxn(ctx context.Context, trxn *pgx.Tx, frConfig FrConfigType) error {
	frConfig.LastModifiedTime = time.Now().UTC().Format(time.RFC3339)
	log.Println("Setting Fundraiding Config (with Trxn): ", frConfig)

	log.Println("Deleting existing record")
	_, err := (*trxn).Exec(context.Background(), "delete from fundraiser_config")
	if err != nil {
		return err
	}

	sqlFields, valIdxs, values := FrConfigType2Sql(frConfig)

	sqlCmd := fmt.Sprintf("insert into fundraiser_config(%s) values (%s)",
		strings.Join(sqlFields, ","), strings.Join(valIdxs, ","))

	log.Println("Setting Config SqlCmd: ", sqlCmd)
	_, err = (*trxn).Exec(context.Background(), sqlCmd, values...)
	if err != nil {
		return err
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////
//
func SetFundraiserConfig(ctx context.Context, frConfig FrConfigType) (bool, error) {
	log.Println("Setting Fundraiding Config: ", frConfig)

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	if err := setFundraiserConfigWithTrxn(ctx, &trxn, frConfig); err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}
	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
func updateFundraiserConfigWithTrxn(ctx context.Context, trxn *pgx.Tx, frConfig FrConfigType) error {
	frConfig.LastModifiedTime = time.Now().UTC().Format(time.RFC3339)
	log.Println("Updating Fundraiding Config (with Trxn): ", frConfig)

	sqlFields, valIdxs, values := FrConfigType2Sql(frConfig)

	updateSqlFlds := []string{}
	for i, f := range sqlFields {
		updateSqlFlds = append(updateSqlFlds, fmt.Sprintf("%s=%s", f, valIdxs[i]))
	}

	sqlCmd := fmt.Sprintf(
		"UPDATE fundraiser_config SET %s WHERE last_modified_time=(SELECT last_modified_time FROM fundraiser_config LIMIT 1)",
		strings.Join(updateSqlFlds, ","))

	log.Println("Update Config SqlCmd: ", sqlCmd)
	_, err := (*trxn).Exec(context.Background(), sqlCmd, values...)
	if err != nil {
		return err
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////
//
func UpdateFundraiserConfig(ctx context.Context, frConfig FrConfigType) (bool, error) {
	frConfig.LastModifiedTime = time.Now().UTC().Format(time.RFC3339)
	log.Println("Updating Fundraiding Config")

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	if err := updateFundraiserConfigWithTrxn(ctx, &trxn, frConfig); err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}

	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type NeighborhoodInfo struct {
	Name              string  `json: name`
	Zipcode           *int    `json: zipcode`
	City              *string `json: city`
	IsVisible         *bool   `json: is_visible`
	DistributionPoint *string `json:"distributionPoint"`
	LastModifiedTime  string  `json:"lastModifiedTime"`
}

////////////////////////////////////////////////////////////////////////////
//
func GetNeighborhoods(gqlFields []string) ([]NeighborhoodInfo, error) {

	log.Println("Retrieving Fundraiser Neighborhoods")

	neighborhoods := []NeighborhoodInfo{}
	sqlFields := []string{}

	for _, gqlField := range gqlFields {
		switch {
		case "name" == gqlField:
			sqlFields = append(sqlFields, "name")
		case "zipcode" == gqlField:
			sqlFields = append(sqlFields, "zipcode")
		case "city" == gqlField:
			sqlFields = append(sqlFields, "city")
		case "isVisible" == gqlField:
			sqlFields = append(sqlFields, "is_visible")
		case "distributionPoint" == gqlField:
			sqlFields = append(sqlFields, "dist_pt")
		default:
			return neighborhoods, errors.New(fmt.Sprintf("Unknown fundraiser neighborhood field: %s", gqlField))
		}

	}

	sqlCmd := fmt.Sprintf("select %s from neighborhoods", strings.Join(sqlFields, ","))
	rows, err := Db.Query(context.Background(), sqlCmd)
	if err != nil {
		log.Println("Neighborhood query failed", err)
		return neighborhoods, err
	}
	defer rows.Close()

	for rows.Next() {
		hood := NeighborhoodInfo{}
		inputs := []interface{}{}
		for _, gqlField := range gqlFields {
			switch {
			case "name" == gqlField:
				inputs = append(inputs, &hood.Name)
			case "zipcode" == gqlField:
				inputs = append(inputs, &hood.Zipcode)
			case "city" == gqlField:
				inputs = append(inputs, &hood.City)
			case "isVisible" == gqlField:
				inputs = append(inputs, &hood.IsVisible)
			case "distributionPoint" == gqlField:
				inputs = append(inputs, &hood.DistributionPoint)
			default:
				return neighborhoods, errors.New(fmt.Sprintf("Unknown fundraiser neighborhood field: %s", gqlField))
			}

		}
		err = rows.Scan(inputs...)
		if err != nil {
			log.Println("Reading Neighborhood row failed: ", err)
			continue
		}
		neighborhoods = append(neighborhoods, hood)
	}

	if rows.Err() != nil {
		log.Println("Reading Neighborhood rows had an issue: ", err)
		return []NeighborhoodInfo{}, err
	}
	return neighborhoods, nil
}

////////////////////////////////////////////////////////////////////////////
//
func FrHoodType2Sql(hood NeighborhoodInfo) ([]string, []string, []interface{}) {
	values := []interface{}{}
	sqlFields := []string{}
	valIdxs := []string{}
	valIdx := 1
	if len(hood.Name) != 0 {
		sqlFields = append(sqlFields, "name")
		values = append(values, hood.Name)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != hood.Zipcode {
		sqlFields = append(sqlFields, "zipcode")
		values = append(values, *hood.Zipcode)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::int", valIdx))
		valIdx++
	}
	if nil != hood.City {
		sqlFields = append(sqlFields, "city")
		values = append(values, *hood.City)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != hood.DistributionPoint {
		sqlFields = append(sqlFields, "dist_pt")
		values = append(values, *hood.DistributionPoint)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
		valIdx++
	}
	if nil != hood.IsVisible {
		// Unfortunately hard to detect if this is set or not
		sqlFields = append(sqlFields, "is_visible")
		values = append(values, *hood.IsVisible)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::bool", valIdx))
		valIdx++
	}

	// Always do timestamp
	sqlFields = append(sqlFields, "last_modified_time")
	values = append(values, hood.LastModifiedTime)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::timestamp", valIdx))
	return sqlFields, valIdxs, values
}

////////////////////////////////////////////////////////////////////////////
//
func AddNeighborhoods(ctx context.Context, hoods []NeighborhoodInfo) (bool, error) {
	lastModifiedTime := time.Now().UTC().Format(time.RFC3339)
	log.Println("Adding Neighborhoods at: ", lastModifiedTime)

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	existingHoods, err := GetNeighborhoods([]string{"name"})
	if err != nil {
		return false, err
	}

	doesAlreadyExist := func(newHood string) bool {
		for _, existingHood := range existingHoods {
			if existingHood.Name == newHood {
				return true
			}
		}
		return false
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	var isDirty = false
	for _, hood := range hoods {
		if doesAlreadyExist(hood.Name) {
			log.Println("Neighborhood: ", hood.Name, " already exists")
			continue
		} else {
			log.Println("Neighborhood: ", hood.Name, " does not exists")
		}
		// log.Println("Deleting existing record if it exists")
		// _, err = trxn.Exec(context.Background(), "delete from users where id = $1", user.Id)
		// if err != nil {
		// 	trxn.Rollback(context.Background())
		// 	return false, err
		// }

		hood.LastModifiedTime = lastModifiedTime
		sqlFields, valIdxs, values := FrHoodType2Sql(hood)

		sqlCmd := fmt.Sprintf("insert into neighborhoods(%s) values (%s)",
			strings.Join(sqlFields, ","), strings.Join(valIdxs, ","))

		log.Println("Adding neighborhood SqlCmd: ", sqlCmd)
		_, err = trxn.Exec(context.Background(), sqlCmd, values...)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
		existingHoods = append(existingHoods, hood)
		isDirty = true
	}

	// Even though neighborhoods aren't a part of the fundariser config table we still treat it like it is so
	// trigger time update to force re-download of config data.

	if isDirty {
		if err := updateFundraiserConfigWithTrxn(ctx, &trxn, FrConfigType{}); err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}

	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type MulchTimecardType struct {
	Id               string `json:"id"`
	DeliveryId       int    `json:"deliveryId"`
	LastModifiedTime string `json:"lastModifiedTime,omitempty"`
	TimeIn           string `json:"timeIn"`
	TimeOut          string `json:"timeOut"`
	TimeTotal        string `json:"timeTotal"`
}

////////////////////////////////////////////////////////////////////////////
//
func mulchTimecardGql2SqlMap(gqlFields []string, tc *MulchTimecardType) ([]string, []interface{}) {

	sqlFields := []string{}
	inputs := []interface{}{}

	for _, gqlField := range gqlFields {
		switch {
		case "id" == gqlField:
			inputs = append(inputs, &tc.Id)
			sqlFields = append(sqlFields, "uid")
		case "deliveryId" == gqlField:
			inputs = append(inputs, &tc.DeliveryId)
			sqlFields = append(sqlFields, "delivery_id")
		case "lastModifiedTime" == gqlField:
			inputs = append(inputs, &tc.LastModifiedTime)
			sqlFields = append(sqlFields, "last_modified_time::string")
		case "timeIn" == gqlField:
			inputs = append(inputs, &tc.TimeIn)
			sqlFields = append(sqlFields, "time_in::string")
		case "timeOut" == gqlField:
			inputs = append(inputs, &tc.TimeOut)
			sqlFields = append(sqlFields, "time_out::string")
		case "timeTotal" == gqlField:
			inputs = append(inputs, &tc.TimeTotal)
			sqlFields = append(sqlFields, "time_total::string")
		default:
			log.Println("Do not know how to handle GraphQL Field: ", gqlField)
		}

	}
	return sqlFields, inputs
}

////////////////////////////////////////////////////////////////////////////
//
func GetMulchTimecards(id string, deliveryId int, gqlFields []string) ([]MulchTimecardType, error) {
	timecards := []MulchTimecardType{}

	tc := MulchTimecardType{} //This is pretty much throw away probably should change to nil
	sqlFields, _ := mulchTimecardGql2SqlMap(gqlFields, &tc)

	sqlCmd := fmt.Sprintf("SELECT %s FROM mulch_delivery_timecards", strings.Join(sqlFields, ","))
	log.Println("Retrieving Timecards: ", sqlCmd)

	values := []interface{}{}
	valIdx := 1
	sqlFields = []string{} // Reset for WHERE entries
	if len(id) != 0 || deliveryId != -1 {
		sqlCmd = sqlCmd + " WHERE"
	}

	if len(id) != 0 {
		log.Println("Timecards User Id: ", id)
		values = append(values, id)
		sqlFields = append(sqlFields, fmt.Sprintf("uid=$%d", valIdx))
		valIdx++
	}
	if deliveryId != -1 {
		log.Println("Timecards DeliveryId: ", deliveryId)
		values = append(values, deliveryId)
		sqlFields = append(sqlFields, fmt.Sprintf("delivery_id=$%d", valIdx))
		valIdx++
	}

	sqlCmd = fmt.Sprintf("%s %s", sqlCmd, strings.Join(sqlFields, " AND "))
	rows, err := Db.Query(context.Background(), sqlCmd, values...)

	if err != nil {
		log.Println("Timecard query Failed", err)
		return timecards, nil
	}

	defer rows.Close()

	for rows.Next() {
		tc := MulchTimecardType{}
		_, inputs := mulchTimecardGql2SqlMap(gqlFields, &tc)
		err = rows.Scan(inputs...)
		if err != nil {
			log.Println("Reading timecard row failed: ", err)
			continue
		}
		timecards = append(timecards, tc)
	}

	if rows.Err() != nil {
		log.Println("Reading timecard rows had an issue: ", err)
		return []MulchTimecardType{}, nil
	}
	return timecards, nil
}

////////////////////////////////////////////////////////////////////////////
//
func SetMulchTimecards(ctx context.Context, timecards []MulchTimecardType) (bool, error) {
	lastModifiedTime := time.Now().UTC().Format(time.RFC3339)
	log.Println("Setting Timecards at: ", lastModifiedTime)

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	for _, timecard := range timecards {
		log.Println("Deleting existing record if it exists: ", timecard.Id)
		_, err = trxn.Exec(context.Background(),
			"delete from mulch_delivery_timecards where uid = $1 and delivery_id = $2",
			timecard.Id, timecard.DeliveryId)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
		if len(timecard.TimeTotal) > 0 && timecard.TimeTotal != "00:00:00" {
			sqlCmd := "insert into mulch_delivery_timecards(uid, delivery_id, last_modified_time, time_in, time_out, time_total) " +
				"values ($1, $2, $3::timestamp, $4::time, $5::time, $6::time)"
			log.Println("Setting Timecard SqlCmd: ", sqlCmd)
			_, err = trxn.Exec(context.Background(), sqlCmd,
				timecard.Id, timecard.DeliveryId, lastModifiedTime, timecard.TimeIn, timecard.TimeOut, timecard.TimeTotal)
			if err != nil {
				trxn.Rollback(context.Background())
				return false, err
			}
		}
	}
	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}

	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type UserInfo struct {
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Id           string `json:"id"`
	Group        string `json:"group"`
	HasAuthCreds bool   `json:"hasAuthCreds,omitempty"`
}

////////////////////////////////////////////////////////////////////////////
//
func GetUsers(gqlFields []string) ([]UserInfo, error) {

	log.Println("Retrieving Fundraiser Users")

	users := []UserInfo{}
	sqlFields := []string{}

	for _, gqlField := range gqlFields {
		switch {
		case "firstName" == gqlField:
			sqlFields = append(sqlFields, "first_name")
		case "lastName" == gqlField:
			sqlFields = append(sqlFields, "last_name")
		case "id" == gqlField:
			sqlFields = append(sqlFields, "id")
		case "group" == gqlField:
			sqlFields = append(sqlFields, "group_id")
		case "hasAuthCreds" == gqlField:
			sqlFields = append(sqlFields, "has_auth_creds")
		default:
			return users, errors.New(fmt.Sprintf("Unknown fundraiser user field: %s", gqlField))
		}

	}

	sqlCmd := fmt.Sprintf("select %s from users", strings.Join(sqlFields, ","))
	rows, err := Db.Query(context.Background(), sqlCmd)
	if err != nil {
		log.Println("User query failed", err)
		return users, err
	}
	defer rows.Close()

	for rows.Next() {
		user := UserInfo{}
		inputs := []interface{}{}
		for _, gqlField := range gqlFields {
			switch {
			case "firstName" == gqlField:
				inputs = append(inputs, &user.FirstName)
			case "lastName" == gqlField:
				inputs = append(inputs, &user.LastName)
			case "id" == gqlField:
				inputs = append(inputs, &user.Id)
			case "group" == gqlField:
				inputs = append(inputs, &user.Group)
			case "hasAuthCreds" == gqlField:
				inputs = append(inputs, &user.HasAuthCreds)
			default:
				return users, errors.New(fmt.Sprintf("Unknown fundraiser user field: %s", gqlField))
			}

		}
		err = rows.Scan(inputs...)
		if err != nil {
			log.Println("Reading User row failed: ", err)
			continue
		}
		users = append(users, user)
	}

	if rows.Err() != nil {
		log.Println("Reading User rows had an issue: ", err)
		return []UserInfo{}, err
	}
	return users, nil
}

////////////////////////////////////////////////////////////////////////////
//
func AddUsers(ctx context.Context, users []UserInfo) (bool, error) {
	lastModifiedTime := time.Now().UTC().Format(time.RFC3339)
	log.Println("Setting Users at: ", lastModifiedTime)

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	existingUsers, err := GetUsers([]string{"id"})
	if err != nil {
		return false, err
	}

	doesAlreadyExist := func(uid string) bool {
		for _, existingUser := range existingUsers {
			if existingUser.Id == uid {
				return true
			}
		}
		return false
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	for _, user := range users {
		if doesAlreadyExist(user.Id) {
			log.Println("User: ", user.Id, " already exists")
			continue
		} else {
			log.Println("User: ", user.Id, " does not exists")
		}
		// log.Println("Deleting existing record if it exists")
		// _, err = trxn.Exec(context.Background(), "delete from users where id = $1", user.Id)
		// if err != nil {
		// 	trxn.Rollback(context.Background())
		// 	return false, err
		// }

		sqlCmd := "insert into users(id, user.first_name, user.last_name, group_id, has_auth_creds, last_modified_time, created_time) " +
			"values ($1, $2, $3, $4, $5::timestamp, $6::timestamp)"
		log.Println("Adding user SqlCmd: ", sqlCmd)
		_, err = trxn.Exec(context.Background(),
			sqlCmd,
			user.Id,
			user.FirstName,
			user.LastName,
			user.Group,
			false,
			lastModifiedTime,
			lastModifiedTime)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
		existingUsers = append(existingUsers, user)
	}

	// Even though users aren't a part of the fundariser config table we still treat it like it is so
	// trigger time update to force re-download of config data.
	if err := updateFundraiserConfigWithTrxn(ctx, &trxn, FrConfigType{}); err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}

	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type NewIssue struct {
	Id    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

////////////////////////////////////////////////////////////////////////////
//
var newIssueGql = `
mutation CreateIssue {
  createIssue(input: {
		repositoryId: "MDEwOlJlcG9zaXRvcnkzMDQ5ODg5MDE=",
		title: "***TITLE***",
		body: "***BODY***",
		labelIds: ["MDU6TGFiZWwyNDM0MzA3ODIy", "LA_kwDOEi3C5c7dGLgb"],
		assigneeIds:["MDQ6VXNlcjM0OTQ5Mg=="]
	}) {
    issue {
      number
      body
    }
  }
}
`

////////////////////////////////////////////////////////////////////////////
//
func CreateIssue(issue NewIssue) (bool, error) {
	url := "https://api.github.com/graphql"

	title := fmt.Sprint("[", issue.Id, "] ", issue.Title)
	newIssueReq := strings.ReplaceAll(newIssueGql, "***TITLE***", title)
	newIssueReq = strings.ReplaceAll(newIssueReq, "***BODY***", issue.Body)

	type GReq struct {
		Query string `json:"query"`
	}
	gqlReq := GReq{Query: newIssueReq}

	reqBytes, err := json.Marshal(gqlReq)
	if err != nil {
		return false, err
	}

	log.Println(string(reqBytes))
	payload := strings.NewReader(string(reqBytes))

	req, err := http.NewRequest("POST", url, payload)
	if err != nil {
		return false, err
	}
	req.Header.Add("content-type", "application/json")
	req.Header.Add("authorization", "Bearer "+os.Getenv("CREATE_ISSUE_TOKEN"))

	res, err := http.DefaultClient.Do(req)

	log.Println(res)
	if err != nil {
		return false, err
	}

	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	log.Println(string(body))

	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
func SetSpreaders(orderId string, spreaders []string) (bool, error) {

	if len(orderId) == 0 {
		return false, errors.New("orderId must be provided")
	}

	// Start Database Operations
	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	log.Println("Deleting existing record")
	_, err = trxn.Exec(context.Background(), "delete from mulch_spreaders where order_id = $1", orderId)
	if err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	if len(spreaders) > 0 {
		sqlCmd := "insert into mulch_spreaders(order_id, spreaders) values ($1, $2::jsonb)"
		_, err = trxn.Exec(context.Background(), sqlCmd, orderId, spreaders)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}
	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
type AllocationItemType struct {
	Uid                       string
	BagsSold                  *int
	BagsSpread                *string
	DeliveryMinutes           *string
	TotalDonations            *string
	AllocationsFromBagsSold   *string
	AllocationsFromBagsSpread *string
	AllocationsFromDelivery   *string
	AllocationsTotal          string
}

////////////////////////////////////////////////////////////////////////////
//
func AllocItemType2Sql(item AllocationItemType) ([]string, []string, []interface{}) {
	values := []interface{}{}
	sqlFields := []string{}
	valIdxs := []string{}
	valIdx := 1

	sqlFields = append(sqlFields, "uid")
	values = append(values, item.Uid)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::string", valIdx))
	valIdx++

	sqlFields = append(sqlFields, "allocation_total")
	values = append(values, item.AllocationsTotal)
	valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
	valIdx++

	if nil != item.BagsSold {
		sqlFields = append(sqlFields, "bags_sold")
		values = append(values, *item.BagsSold)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::int", valIdx))
		valIdx++
	}

	if nil != item.BagsSpread {
		sqlFields = append(sqlFields, "bags_spread")
		values = append(values, *item.BagsSpread)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	if nil != item.DeliveryMinutes {
		sqlFields = append(sqlFields, "delivery_minutes")
		values = append(values, *item.DeliveryMinutes)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	if nil != item.TotalDonations {
		sqlFields = append(sqlFields, "total_donations")
		values = append(values, *item.TotalDonations)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	if nil != item.AllocationsFromBagsSold {
		sqlFields = append(sqlFields, "allocation_from_bags_sold")
		values = append(values, *item.AllocationsFromBagsSold)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	if nil != item.AllocationsFromBagsSpread {
		sqlFields = append(sqlFields, "allocation_from_bags_spread")
		values = append(values, *item.AllocationsFromBagsSpread)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	if nil != item.AllocationsFromDelivery {
		sqlFields = append(sqlFields, "allocation_from_delivery")
		values = append(values, *item.AllocationsFromDelivery)
		valIdxs = append(valIdxs, fmt.Sprintf("$%d::decimal", valIdx))
		valIdx++
	}

	return sqlFields, valIdxs, values
}

////////////////////////////////////////////////////////////////////////////
//
func SetFrCloseoutAllocations(ctx context.Context, allocations []AllocationItemType) (bool, error) {
	log.Println("Setting Fr Closeout Allocations: ", allocations)

	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	log.Println("Deleting existing records")
	_, err = trxn.Exec(context.Background(), "delete from allocation_summary")
	if err != nil {
		trxn.Rollback(context.Background())
		return false, err
	}

	for _, item := range allocations {
		if len(item.Uid) == 0 {
			trxn.Rollback(context.Background())
			errMsg := fmt.Sprint("UID not in record: ", item)
			log.Println(errMsg)
			return false, errors.New(errMsg)
		}
		sqlFields, valIdxs, values := AllocItemType2Sql(item)
		sqlCmd := fmt.Sprintf("insert into allocation_summary(%s) values (%s)",
			strings.Join(sqlFields, ","), strings.Join(valIdxs, ","))
		log.Println("Adding Allocation for ", item.Uid, " SqlCmd: ", sqlCmd)
		_, err = trxn.Exec(context.Background(), sqlCmd, values...)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}

	}
	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}
	return true, nil
}

const DROP_ORDER_TABLE_SQL = "drop table allocation_summary, mulch_delivery_timecards, mulch_orders, mulch_spreaders"
const MULCH_ORDERS_TABLE_SQL = "CREATE TABLE mulch_orders (order_id UUID PRIMARY KEY DEFAULT gen_random_uuid(), order_owner_id STRING, cash_amount_collected DECIMAL(13, 4), check_amount_collected DECIMAL(13, 4), check_numbers STRING, amount_from_donations DECIMAL(13, 4), amount_from_purchases DECIMAL(13, 4), will_collect_money_later BOOL, total_amount_collected DECIMAL(13,4), special_instructions STRING, is_verified BOOL, last_modified_time TIMESTAMP, purchases JSONB, delivery_id INT, customer_addr1 STRING, customer_addr2 STRING, customer_neighborhood STRING, known_addr_id UUID, customer_email STRING, customer_phone STRING, customer_name STRING)"
const MULCH_SPREADERS_TABLE_SQL = "CREATE TABLE mulch_spreaders (order_id UUID PRIMARY KEY, spreaders JSONB)"
const MULCH_DELIVERY_TIMECARD_TABLE_SQL = "CREATE TABLE mulch_delivery_timecards (uid STRING, delivery_id INT, last_modified_time TIMESTAMP, time_in TIME, time_out TIME, time_total TIME, PRIMARY KEY (uid, delivery_id, time_in))"
const ALLOCATION_SUMMARY_TABLE_SQL = "CREATE TABLE allocation_summary (uid STRING PRIMARY KEY, bags_sold INT, bags_spread DECIMAL(13,4), delivery_minutes DECIMAL(13,4), total_donations DECIMAL(13,4), allocation_from_bags_sold DECIMAL(13,4), allocation_from_bags_spread DECIMAL(13,4), allocation_from_delivery DECIMAL(13,4), allocation_total DECIMAL(13,4))"

const DROP_USERS_TABLE_SQL = "drop table users"
const USERS_TABLE_SQL = "CREATE TABLE users (id STRING, group_id STRING, name STRING, created_time TIMESTAMP, last_modified_time TIMESTAMP, has_auth_creds BOOL)"

////////////////////////////////////////////////////////////////////////////
//
func resetOrderTables(ctx context.Context, trxn *pgx.Tx) error {
	resetSqlCmds := [...]string{
		DROP_ORDER_TABLE_SQL,
		MULCH_ORDERS_TABLE_SQL,
		MULCH_SPREADERS_TABLE_SQL,
		MULCH_DELIVERY_TIMECARD_TABLE_SQL,
		ALLOCATION_SUMMARY_TABLE_SQL,
	}

	for _, sqlCmd := range resetSqlCmds {
		if _, err := (*trxn).Exec(context.Background(), sqlCmd); err != nil {
			return err
		}
	}
	return nil

}

////////////////////////////////////////////////////////////////////////////
//
func ResetFundraisingData(ctx context.Context, doResetUsers bool, doResetOrders bool) (bool, error) {
	log.Println("Setting Fr Data: users: %t  doResetOrders: %t", doResetUsers, doResetOrders)

	if !(doResetUsers || doResetOrders) {
		// Was told not to reset anything
		return true, nil
	}
	if err := VerifyAdminTokenFromCtx(ctx); err != nil {
		return false, err
	}

	trxn, err := Db.Begin(context.Background())
	if err != nil {
		return false, err
	}

	if doResetUsers {
		log.Println("Resetting users data ")
		_, err = trxn.Exec(context.Background(), DROP_USERS_TABLE_SQL)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
		_, err = trxn.Exec(context.Background(), USERS_TABLE_SQL)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
	}

	if doResetOrders {
		log.Println("Resetting orders data")
		err = resetOrderTables(ctx, &trxn)
		if err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}

		frConfig := FrConfigType{FinalizationData: &FinalizationDataType{}, MulchDeliveryConfigs: &[]MulchDeliveryConfigType{}}
		if err := updateFundraiserConfigWithTrxn(ctx, &trxn, frConfig); err != nil {
			trxn.Rollback(context.Background())
			return false, err
		}
	}

	log.Println("About to make a commitment")
	err = trxn.Commit(context.Background())
	if err != nil {
		return false, err
	}
	return true, nil
}

////////////////////////////////////////////////////////////////////////////
//
// func AdminTestApi(ctx context.Context, param1 string) (bool, error) {
// 	log.Println("Admin Test API")
//
// 	if err := verifyUidAllowedFromCtx(ctx, param1); err != nil {
// 		return false, err
// 	}
// 	log.Println("Worked")
// 	return true, nil
// }
