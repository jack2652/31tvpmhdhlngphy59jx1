package main
import ("fmt"; "option-scope-go/internal/store")
func main(){ db,e:=store.Open("data/options.db");if e!=nil{panic(e)};defer db.Close(); for _,s:=range []string{"QQQ","SPY","AAPL","MSFT","NVDA"}{r,e:=db.LatestIntradayHistory(s,"15m",10000);if e!=nil{fmt.Println(s,e);continue}; b,_:=r["bars"].([]map[string]any); first,last:="","";if len(b)>0{first,_=b[0]["date"].(string);last,_=b[len(b)-1]["date"].(string)};fmt.Println(s,len(b),first,last)}}
