// Weather Overground: hybrid Weather & AQI data collector.
// Sources: IQAir (station), Tomorrow.io (1 km model), OpenAQ (aggregator), Open-Meteo (CAMS baseline).
// Config: JSON.  Storage: one append-only CSV per source.  No external dependencies.
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
	"path/filepath"
	"strings"
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
		Dir string `json:"dir"`
	} `json:"storage"`
	APIKeys struct {
		TomorrowIO string `json:"tomorrow_io"`
		IQAir      string `json:"iqair"`
		OpenAQ     string `json:"openaq"`
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
	if cfg.Storage.Dir == "" {
		cfg.Storage.Dir = "./data"
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

func getJSONWithHeaders(rawURL string, params map[string]string, headers map[string]string, dest any) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
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
// Shared helpers
// ---------------------------------------------------------------------------

func ptrF(f float64) *float64 { return &f }
func ptrI(i int) *int         { return &i }

func fmtF(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', 4, 64)
}

func fmtI(i *int) string {
	if i == nil {
		return ""
	}
	return strconv.Itoa(*i)
}

// ---------------------------------------------------------------------------
// CSV writer
// ---------------------------------------------------------------------------

// appendCSV opens (or creates) a CSV at path and appends rows to it.
// The header is written only on first creation.
func appendCSV(path string, header []string, rows [][]string) error {
	fileExists := true
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fileExists = false
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if !fileExists {
		if err := w.Write(header); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ---------------------------------------------------------------------------
// IQAir
// ---------------------------------------------------------------------------

type IQAirRow struct {
	TimestampUTC string
	LocationName string
	AQIUS        *int
	Temp         *float64
	Humidity     *float64
}

var iqairHeader = []string{
	"timestamp_utc", "location_name",
	"aqi_us", "temp_c", "humidity_pct",
}

func (r IQAirRow) toCSV() []string {
	return []string{
		r.TimestampUTC, r.LocationName,
		fmtI(r.AQIUS), fmtF(r.Temp), fmtF(r.Humidity),
	}
}

func fetchIQAir(loc Location, apiKey string) (IQAirRow, error) {
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
		return IQAirRow{}, err
	}

	c := result.Data.Current
	return IQAirRow{
		AQIUS:    ptrI(c.Pollution.AQIUS),
		Temp:     ptrF(c.Weather.Tp),
		Humidity: ptrF(c.Weather.Hu),
	}, nil
}

// ---------------------------------------------------------------------------
// Tomorrow.io
// ---------------------------------------------------------------------------

type TomorrowRow struct {
	TimestampUTC string
	LocationName string
	Temp         *float64
	TempApparent *float64
	Humidity     *float64
	WindSpeed    *float64
	WindDir      *float64
	Pressure     *float64
	Rain         *float64
	UVIndex      *float64
}

var tomorrowHeader = []string{
	"timestamp_utc", "location_name",
	"temp_c", "temp_apparent_c", "humidity_pct",
	"wind_speed_ms", "wind_dir_deg", "pressure_hpa",
	"rain_mm", "uv_index",
}

func (r TomorrowRow) toCSV() []string {
	return []string{
		r.TimestampUTC, r.LocationName,
		fmtF(r.Temp), fmtF(r.TempApparent), fmtF(r.Humidity),
		fmtF(r.WindSpeed), fmtF(r.WindDir), fmtF(r.Pressure),
		fmtF(r.Rain), fmtF(r.UVIndex),
	}
}

func fetchTomorrow(loc Location, apiKey string) (TomorrowRow, error) {
	var result struct {
		Data struct {
			Values struct {
				Temp         float64 `json:"temperature"`
				TempAppar    float64 `json:"temperatureApparent"`
				Humidity     float64 `json:"humidity"`
				WindSpeed    float64 `json:"windSpeed"`
				WindDir      float64 `json:"windDirection"`
				Pressure     float64 `json:"pressureSurfaceLevel"`
				Rain         float64 `json:"rainAccumulation"`
				UVIndex      float64 `json:"uvIndex"`
			} `json:"values"`
		} `json:"data"`
	}

	err := getJSON("https://api.tomorrow.io/v4/weather/realtime", map[string]string{
		"location": fmt.Sprintf("%f,%f", loc.Lat, loc.Lon),
		"fields":   "temperature,temperatureApparent,humidity,windSpeed,windDirection,pressureSurfaceLevel,rainAccumulation,uvIndex",
		"units":    "metric",
		"apikey":   apiKey,
	}, &result)
	if err != nil {
		return TomorrowRow{}, err
	}

	v := result.Data.Values
	return TomorrowRow{
		Temp:         ptrF(v.Temp),
		TempApparent: ptrF(v.TempAppar),
		Humidity:     ptrF(v.Humidity),
		WindSpeed:    ptrF(v.WindSpeed),
		WindDir:      ptrF(v.WindDir),
		Pressure:     ptrF(v.Pressure),
		Rain:         ptrF(v.Rain),
		UVIndex:      ptrF(v.UVIndex),
	}, nil
}

// ---------------------------------------------------------------------------
// OpenAQ
// ---------------------------------------------------------------------------

type OpenAQRow struct {
	TimestampUTC string
	LocationName string
	StationName  string
	PM25         *float64
	PM10         *float64
	O3           *float64
	NO2          *float64
}

var openAQHeader = []string{
	"timestamp_utc", "location_name", "station_name",
	"pm25_ugm3", "pm10_ugm3", "o3_ugm3", "no2_ugm3",
}

func (r OpenAQRow) toCSV() []string {
	return []string{
		r.TimestampUTC, r.LocationName, r.StationName,
		fmtF(r.PM25), fmtF(r.PM10), fmtF(r.O3), fmtF(r.NO2),
	}
}

func fetchOpenAQ(loc Location, apiKey string) (OpenAQRow, error) {
	headers := map[string]string{"X-API-Key": apiKey}

	// Step 1: Find the nearest station metadata
	var locResult struct {
		Results []struct {
			ID      int    `json:"id"`
			Name    string `json:"name"`
			Sensors []struct {
				ID        int `json:"id"`
				Parameter struct {
					Name string `json:"name"`
				} `json:"parameter"`
			} `json:"sensors"`
		} `json:"results"`
	}
	err := getJSONWithHeaders(
		"https://api.openaq.org/v3/locations",
		map[string]string{
			"coordinates":   fmt.Sprintf("%f,%f", loc.Lat, loc.Lon),
			"radius":        "25000",
			"limit":         "5",
			"order_by":      "id",
			"parameters_id": "2", // Must have PM2.5 capability
		},
		headers,
		&locResult,
	)
	if err != nil {
		return OpenAQRow{}, fmt.Errorf("locations lookup: %w", err)
	}
	if len(locResult.Results) == 0 {
		return OpenAQRow{}, fmt.Errorf("no stations found within 25km")
	}

	station := locResult.Results[0]

	// Dynamically build a map of sensorID -> parameterName from Step 1
	sensorMap := make(map[int]string)
	for _, s := range station.Sensors {
		sensorMap[s.ID] = s.Parameter.Name
	}

	// Step 2: Get flat latest measurements for that station ID
	var latest struct {
		Results []struct {
			SensorsID int     `json:"sensorsId"`
			Value     float64 `json:"value"`
			Datetime  struct {
				Utc string `json:"utc"`
			} `json:"datetime"`
		} `json:"results"`
	}
	err = getJSONWithHeaders(
		fmt.Sprintf("https://api.openaq.org/v3/locations/%d/latest", station.ID),
		nil,
		headers,
		&latest,
	)
	if err != nil {
		return OpenAQRow{}, fmt.Errorf("latest measurements: %w", err)
	}

	row := OpenAQRow{
		LocationName: loc.Name, // Pull the configured lookup label
		StationName:  station.Name,
	}
	
	hasFreshData := false

	for _, m := range latest.Results {
		// Filter out stale records (like the 2021 anomalies we saw)
		if !strings.HasPrefix(m.Datetime.Utc, "2026-05") {
			continue
		}

		// Identify parameter name using our dynamic look-up map
		paramName, exists := sensorMap[m.SensorsID]
		if !exists {
			continue
		}

		v := m.Value
		hasFreshData = true
		
		// Map the record timestamp dynamically from the first valid sensor found
		if row.TimestampUTC == "" {
			row.TimestampUTC = m.Datetime.Utc
		}

		switch paramName {
		case "pm25":
			row.PM25 = &v
		case "pm10":
			row.PM10 = &v
		case "o3":
			row.O3 = &v
		case "no2":
			row.NO2 = &v
		}
	}

	if !hasFreshData {
		return OpenAQRow{}, fmt.Errorf("station found, but data is completely stale")
	}

	return row, nil
}

// ---------------------------------------------------------------------------
// Open-Meteo (CAMS, no key)
// ---------------------------------------------------------------------------

type OpenMeteoRow struct {
	TimestampUTC string
	LocationName string
	PM25         *float64
	PM10         *float64
	O3           *float64
}

var openMeteoHeader = []string{
	"timestamp_utc", "location_name",
	"pm25_ugm3", "pm10_ugm3", "o3_ugm3",
}

func (r OpenMeteoRow) toCSV() []string {
	return []string{
		r.TimestampUTC, r.LocationName,
		fmtF(r.PM25), fmtF(r.PM10), fmtF(r.O3),
	}
}

func fetchOpenMeteo(loc Location) (OpenMeteoRow, error) {
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
		return OpenMeteoRow{}, err
	}

	return OpenMeteoRow{
		PM25: ptrF(result.Current.PM25),
		PM10: ptrF(result.Current.PM10),
		O3:   ptrF(result.Current.Ozone),
	}, nil
}

// ---------------------------------------------------------------------------
// Collection loop
// ---------------------------------------------------------------------------

type collectionResults struct {
	iqair     []IQAirRow
	tomorrow  []TomorrowRow
	openMeteo []OpenMeteoRow
	openAQ    []OpenAQRow
}

func collect(cfg Config, logger *log.Logger) collectionResults {
	ts := time.Now().UTC().Format(time.RFC3339)
	var r collectionResults

	for _, loc := range cfg.Locations {
		logger.Printf("── %s (%.4f, %.4f)", loc.Name, loc.Lat, loc.Lon)

		if row, err := fetchIQAir(loc, cfg.APIKeys.IQAir); err != nil {
			logger.Printf("  [IQAir] ERROR: %v", err)
		} else {
			row.TimestampUTC = ts
			row.LocationName = loc.Name
			r.iqair = append(r.iqair, row)
			logger.Printf("  [IQAir] OK")
		}

		if row, err := fetchTomorrow(loc, cfg.APIKeys.TomorrowIO); err != nil {
			logger.Printf("  [Tomorrow.io] ERROR: %v", err)
		} else {
			row.TimestampUTC = ts
			row.LocationName = loc.Name
			r.tomorrow = append(r.tomorrow, row)
			logger.Printf("  [Tomorrow.io] OK")
		}

		if row, err := fetchOpenAQ(loc, cfg.APIKeys.OpenAQ); err != nil {
			logger.Printf("  [OpenAQ] ERROR: %v", err)
		} else {
			row.TimestampUTC = ts
			row.LocationName = loc.Name
			r.openAQ = append(r.openAQ, row)
			logger.Printf("  [OpenAQ] OK")
		}

		if row, err := fetchOpenMeteo(loc); err != nil {
			logger.Printf("  [Open-Meteo/CAMS] ERROR: %v", err)
		} else {
			row.TimestampUTC = ts
			row.LocationName = loc.Name
			r.openMeteo = append(r.openMeteo, row)
			logger.Printf("  [Open-Meteo/CAMS] OK")
		}
	}

	return r
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func persist(dir string, r collectionResults, logger *log.Logger) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		logger.Fatalf("Cannot create data dir %s: %v", dir, err)
	}

	type writeJob struct {
		filename string
		header   []string
		rows     [][]string
	}

	var iqairRows, tomorrowRows, openAQRows, openMeteoRows [][]string
	for _, row := range r.iqair {
		iqairRows = append(iqairRows, row.toCSV())
	}
	for _, row := range r.tomorrow {
		tomorrowRows = append(tomorrowRows, row.toCSV())
	}
	for _, row := range r.openMeteo {
		openMeteoRows = append(openMeteoRows, row.toCSV())
	}
	for _, row := range r.openAQ {
		openAQRows = append(openAQRows, row.toCSV())
	}

	jobs := []writeJob{
		{"iqair.csv", iqairHeader, iqairRows},
		{"tomorrow.csv", tomorrowHeader, tomorrowRows},
		{"openmeteo.csv", openMeteoHeader, openMeteoRows},
		{"openaq.csv", openAQHeader, openAQRows},
	}

	for _, job := range jobs {
		if len(job.rows) == 0 {
			continue
		}
		path := filepath.Join(dir, job.filename)
		if err := appendCSV(path, job.header, job.rows); err != nil {
			logger.Printf("CSV write error (%s): %v", job.filename, err)
		} else {
			logger.Printf("Wrote %d row(s) → %s", len(job.rows), path)
		}
	}
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "config.json", "Path to JSON config file")
	flag.Parse()

	logFile, err := os.OpenFile("overground.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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

	r := collect(cfg, logger)
	persist(cfg.Storage.Dir, r, logger)

	logger.Println("Done.")
}
