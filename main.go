// weather-overground: hybrid Weather & AQI data collector.
// Sources: IQAir (station), Tomorrow.io (1 km model), Open-Meteo (CAMS baseline).
// Config: JSON.  Storage: append-only CSV.  No external dependencies.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Location struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type Config struct {
	Storage struct {
		Path string `json:"path"`
	} `json:"storage"`
	APIKeys struct {
		TomorrowIO string `json:"tomorrow_io"`
		IQAir      string `json:"iqair"`
	} `json:"api_keys"`
	Locations []Location `json:"locations"`
}

func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "./weather_data.csv"
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// HTTP helper
// ---------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 15 * time.Second}

func getJSON(rawURL string, params map[string]string, dest any) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	resp, err := httpClient.Get(u.String())
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, dest)
}

// ---------------------------------------------------------------------------
// Row: the normalised output type
// ---------------------------------------------------------------------------

type Row struct {
	TimestampUTC string
	LocationName string
	Source       string
	PM25         *float64
	PM10         *float64
	O3           *float64
	Temp         *float64
	Humidity     *float64
	PollenTree   *float64
}

func fmtF(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', 4, 64)
}

func (r Row) ToCSV() []string {
	return []string{
		r.TimestampUTC,
		r.LocationName,
		r.Source,
		fmtF(r.PM25),
		fmtF(r.PM10),
		fmtF(r.O3),
		fmtF(r.Temp),
		fmtF(r.Humidity),
		fmtF(r.PollenTree),
	}
}

var csvHeader = []string{
	"timestamp_utc", "location_name", "source",
	"pm25", "pm10", "o3", "temp", "humidity", "pollen_tree",
}

func ptr(f float64) *float64 { return &f }

// ---------------------------------------------------------------------------
// IQAir
// ---------------------------------------------------------------------------

func fetchIQAir(loc Location, apiKey string) (Row, error) {
	var result struct {
		Data struct {
			Current struct {
				Pollution struct {
					AQIUS int `json:"aqius"`
				} `json:"pollution"`
				Weather struct {
					Tp float64 `json:"tp"`
					Hu float64 `json:"hu"`
				} `json:"weather"`
			} `json:"current"`
		} `json:"data"`
	}

	err := getJSON("https://api.airvisual.com/v2/nearest_city", map[string]string{
		"lat": strconv.FormatFloat(loc.Lat, 'f', 6, 64),
		"lon": strconv.FormatFloat(loc.Lon, 'f', 6, 64),
		"key": apiKey,
	}, &result)
	if err != nil {
		return Row{}, err
	}

	aqius := float64(result.Data.Current.Pollution.AQIUS)
	return Row{
		Source:   "iqair",
		PM25:     &aqius, // free tier returns US AQI, not raw µg/m³
		Temp:     ptr(result.Data.Current.Weather.Tp),
		Humidity: ptr(result.Data.Current.Weather.Hu),
	}, nil
}

// ---------------------------------------------------------------------------
// Tomorrow.io
// ---------------------------------------------------------------------------

func fetchTomorrow(loc Location, apiKey string) (Row, error) {
	var result struct {
		Data struct {
			Values struct {
				PM25       float64 `json:"particulateMatter25"`
				PM10       float64 `json:"particulateMatter10"`
				O3         float64 `json:"pollutantO3"`
				Temp       float64 `json:"temperature"`
				Humidity   float64 `json:"humidity"`
				PollenTree float64 `json:"treeIndex"`
			} `json:"values"`
		} `json:"data"`
	}

	err := getJSON("https://api.tomorrow.io/v4/weather/realtime", map[string]string{
		"location": fmt.Sprintf("%f,%f", loc.Lat, loc.Lon),
		"fields":   "particulateMatter25,particulateMatter10,pollutantO3,temperature,humidity,treeIndex",
		"units":    "metric",
		"apikey":   apiKey,
	}, &result)
	if err != nil {
		return Row{}, err
	}

	v := result.Data.Values
	return Row{
		Source:     "tomorrow_io",
		PM25:       ptr(v.PM25),
		PM10:       ptr(v.PM10),
		O3:         ptr(v.O3),
		Temp:       ptr(v.Temp),
		Humidity:   ptr(v.Humidity),
		PollenTree: ptr(v.PollenTree),
	}, nil
}

// ---------------------------------------------------------------------------
// Open-Meteo (CAMS, no key)
// ---------------------------------------------------------------------------

func fetchOpenMeteo(loc Location) (Row, error) {
	var result struct {
		Current struct {
			PM25  float64 `json:"pm2_5"`
			PM10  float64 `json:"pm10"`
			Ozone float64 `json:"ozone"`
		} `json:"current"`
	}

	err := getJSON("https://air-quality-api.open-meteo.com/v1/air-quality", map[string]string{
		"latitude":  strconv.FormatFloat(loc.Lat, 'f', 6, 64),
		"longitude": strconv.FormatFloat(loc.Lon, 'f', 6, 64),
		"current":   "pm2_5,pm10,ozone",
		"timezone":  "UTC",
	}, &result)
	if err != nil {
		return Row{}, err
	}

	return Row{
		Source: "open_meteo_cams",
		PM25:   ptr(result.Current.PM25),
		PM10:   ptr(result.Current.PM10),
		O3:     ptr(result.Current.Ozone),
	}, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func appendCSV(path string, rows []Row) error {
	fileExists := true
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fileExists = false
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if !fileExists {
		if err := w.Write(csvHeader); err != nil {
			return err
		}
	}
	for _, r := range rows {
		if err := w.Write(r.ToCSV()); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ---------------------------------------------------------------------------
// Collection loop
// ---------------------------------------------------------------------------

func collect(cfg Config, logger *log.Logger) []Row {
	ts := time.Now().UTC().Format(time.RFC3339)
	var rows []Row

	stamp := func(r Row, loc Location) Row {
		r.TimestampUTC = ts
		r.LocationName = loc.Name
		return r
	}

	for _, loc := range cfg.Locations {
		logger.Printf("── %s (%.4f, %.4f)", loc.Name, loc.Lat, loc.Lon)

		if r, err := fetchIQAir(loc, cfg.APIKeys.IQAir); err != nil {
			logger.Printf("  [IQAir] ERROR: %v", err)
		} else {
			rows = append(rows, stamp(r, loc))
			logger.Printf("  [IQAir] OK")
		}

		if r, err := fetchTomorrow(loc, cfg.APIKeys.TomorrowIO); err != nil {
			logger.Printf("  [Tomorrow.io] ERROR: %v", err)
		} else {
			rows = append(rows, stamp(r, loc))
			logger.Printf("  [Tomorrow.io] OK")
		}

		if r, err := fetchOpenMeteo(loc); err != nil {
			logger.Printf("  [Open-Meteo/CAMS] ERROR: %v", err)
		} else {
			rows = append(rows, stamp(r, loc))
			logger.Printf("  [Open-Meteo/CAMS] OK")
		}
	}

	return rows
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "config.json", "Path to JSON config file")
	flag.Parse()

	// Set up file logger (also mirrors to stdout via MultiWriter)
	logFile, err := os.OpenFile("collector.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	logger := log.New(io.MultiWriter(logFile, os.Stdout), "", log.LstdFlags)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatalf("Config error: %v", err)
	}

	rows := collect(cfg, logger)

	if len(rows) == 0 {
		logger.Println("No data collected — nothing written.")
		return
	}

	if err := appendCSV(cfg.Storage.Path, rows); err != nil {
		logger.Fatalf("CSV write error: %v", err)
	}

	logger.Printf("Done. %d rows written to %s.", len(rows), cfg.Storage.Path)
}
