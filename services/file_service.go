package services

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"os"
	"stockbackend/clients/http_client"
	mongo_client "stockbackend/clients/mongo"
	"stockbackend/utils/constants"
	"stockbackend/utils/helpers"
	"strings"

	"github.com/cloudinary/cloudinary-go/v2"
	"github.com/cloudinary/cloudinary-go/v2/api/uploader"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
	"gopkg.in/mgo.v2/bson"
)

type FileServiceI interface {
	ParseXLSXFile(ctx *gin.Context, files []*multipart.FileHeader) error
}

type fileService struct{}

var FileService FileServiceI = &fileService{}

func (fs *fileService) ParseXLSXFile(ctx *gin.Context, files []*multipart.FileHeader) error {
	cld, err := cloudinary.NewFromURL(os.Getenv("CLOUDINARY_URL"))
	if err != nil {
		return fmt.Errorf("error initializing Cloudinary: %w", err)
	}
	// Iterate over the uploaded XLSX files
	for _, fileHeader := range files {
		// Open each file for processing
		file, err := fileHeader.Open()
		if err != nil {
			zap.L().Error("Error opening file", zap.String("filename", fileHeader.Filename), zap.Error(err))
			continue
		}
		defer file.Close()

		// Generate a UUID for the filename
		uuid := uuid.New().String()
		cloudinaryFilename := uuid + ".xlsx"

		// Upload file to Cloudinary
		uploadResult, err := cld.Upload.Upload(ctx, file, uploader.UploadParams{
			PublicID: cloudinaryFilename,
			Folder:   "xlsx_uploads",
		})
		if err != nil {
			zap.L().Error("Error uploading file to Cloudinary", zap.String("filename", fileHeader.Filename), zap.Error(err))
			continue
		}

		zap.L().Info("File uploaded to Cloudinary", zap.String("filename", fileHeader.Filename), zap.String("url", uploadResult.SecureURL))

		// Create a new reader from the uploaded file
		file.Seek(0, 0) // Reset file pointer to the beginning
		f, err := excelize.OpenReader(file)
		if err != nil {
			zap.L().Error("Error parsing XLSX file", zap.String("filename", fileHeader.Filename), zap.Error(err))
			continue
		}
		defer f.Close()

		// Get all the sheet names
		sheetList := f.GetSheetList()
		// Loop through the sheets and extract relevant information
		for _, sheet := range sheetList {
			zap.L().Info("Processing file", zap.String("filename", fileHeader.Filename), zap.String("sheet", sheet))

			// Get all the rows in the sheet
			rows, err := f.GetRows(sheet)
			if err != nil {
				zap.L().Error("Error reading rows from sheet", zap.String("sheet", sheet), zap.Error(err))
				continue
			}

			headerFound := false
			headerMap := make(map[string]int)
			stopExtracting := false

			// Loop through the rows in the sheet
			for _, row := range rows {
				if len(row) == 0 {
					continue
				}

				if !headerFound {
					for _, cell := range row {
						if helpers.MatchHeader(cell, []string{`name\s*of\s*(the)?\s*instrument`}) {
							headerFound = true
							// Build the header map
							for i, headerCell := range row {
								normalizedHeader := helpers.NormalizeString(headerCell)
								// Map possible variations to standard keys
								switch {
								case helpers.MatchHeader(normalizedHeader, []string{`name\s*of\s*(the)?\s*instrument`}):
									headerMap["Name of the Instrument"] = i
								case helpers.MatchHeader(normalizedHeader, []string{`isin`}):
									headerMap["ISIN"] = i
								case helpers.MatchHeader(normalizedHeader, []string{`rating\s*/\s*industry`, `industry\s*/\s*rating`}):
									headerMap["Industry/Rating"] = i
								case helpers.MatchHeader(normalizedHeader, []string{`quantity`}):
									headerMap["Quantity"] = i
								case helpers.MatchHeader(normalizedHeader, []string{`market\s*/\s*fair\s*value.*`, `market\s*value.*`}):
									headerMap["Market/Fair Value"] = i
								case helpers.MatchHeader(normalizedHeader, []string{`%.*nav`, `%.*net\s*assets`}):
									headerMap["Percentage of AUM"] = i
								}
							}
							// zap.L().Info("Header found", zap.Any("headerMap", headerMap))
							break
						}
					}
					continue
				}

				// Check for the end marker "Subtotal" or "Total"
				joinedRow := strings.Join(row, "")
				if strings.Contains(strings.ToLower(joinedRow), "subtotal") || strings.Contains(strings.ToLower(joinedRow), "total") {
					stopExtracting = true
					break
				}

				if !stopExtracting {
					stockDetail := make(map[string]interface{})

					// Extract data using the header map
					for key, idx := range headerMap {
						if idx < len(row) {
							stockDetail[key] = row[idx]
						} else {
							stockDetail[key] = ""
						}
					}

					// Check if the stockDetail has meaningful data
					if stockDetail["Name of the Instrument"] == nil || stockDetail["Name of the Instrument"] == "" {
						continue
					}

					// Additional processing
					instrumentName, ok := stockDetail["Name of the Instrument"].(string)
					if !ok {
						continue
					}

					// Apply mapping if exists
					if mappedName, exists := constants.MapValues[instrumentName]; exists {
						stockDetail["Name of the Instrument"] = mappedName
						instrumentName = mappedName
					}

					// Clean up the query string
					queryString := instrumentName
					queryString = strings.ReplaceAll(queryString, " Corporation ", " Corpn ")
					queryString = strings.ReplaceAll(queryString, " corporation ", " Corpn ")
					queryString = strings.ReplaceAll(queryString, " Limited", " Ltd ")
					queryString = strings.ReplaceAll(queryString, " limited", " Ltd ")
					queryString = strings.ReplaceAll(queryString, " and ", " & ")
					queryString = strings.ReplaceAll(queryString, " And ", " & ")

					// Prepare the text search filter
					textSearchFilter := bson.M{
						"$text": bson.M{
							"$search": queryString,
						},
					}

					// MongoDB collection
					collection := mongo_client.Client.Database(os.Getenv("DATABASE")).Collection(os.Getenv("COLLECTION"))

					// Set find options
					findOptions := options.FindOne()
					findOptions.SetProjection(bson.M{
						"score": bson.M{"$meta": "textScore"},
					})
					findOptions.SetSort(bson.M{
						"score": bson.M{"$meta": "textScore"},
					})

					// Perform the search
					var result bson.M
					err = collection.FindOne(context.TODO(), textSearchFilter, findOptions).Decode(&result)
					if err != nil {
						zap.L().Error("Error finding document", zap.Error(err))
						continue
					}

					// Process based on the score
					if score, ok := result["score"].(float64); ok {
						if score >= 1 {
							// zap.L().Info("marketCap", zap.Any("marketCap", result["marketCap"]), zap.Any("name", stockDetail["Name of the Instrument"]))
							stockDetail["marketCapValue"] = result["marketCap"]
							stockDetail["url"] = result["url"]
							stockDetail["marketCap"] = helpers.GetMarketCapCategory(fmt.Sprintf("%v", result["marketCap"]))
							stockDetail["stockRate"] = helpers.RateStock(result)
						} else {
							// zap.L().Info("score less than 1", zap.Float64("score", score))
							results, err := http_client.SearchCompany(instrumentName)
							if err != nil || len(results) == 0 {
								zap.L().Error("No company found", zap.Error(err))
								continue
							}
							data, err := helpers.FetchCompanyData(results[0].URL)
							if err != nil {
								zap.L().Error("Error fetching company data", zap.Error(err))
								continue
							}
							// Update MongoDB with fetched data
							update := bson.M{
								"$set": bson.M{
									"marketCap":           data["Market Cap"],
									"currentPrice":        data["Current Price"],
									"highLow":             data["High / Low"],
									"stockPE":             data["Stock P/E"],
									"bookValue":           data["Book Value"],
									"dividendYield":       data["Dividend Yield"],
									"roce":                data["ROCE"],
									"roe":                 data["ROE"],
									"faceValue":           data["Face Value"],
									"pros":                data["pros"],
									"cons":                data["cons"],
									"quarterlyResults":    data["quarterlyResults"],
									"profitLoss":          data["profitLoss"],
									"balanceSheet":        data["balanceSheet"],
									"cashFlows":           data["cashFlows"],
									"ratios":              data["ratios"],
									"shareholdingPattern": data["shareholdingPattern"],
									"peersTable":          data["peersTable"],
									"peers":               data["peers"],
								},
							}
							updateOptions := options.Update().SetUpsert(true)
							filter := bson.M{"name": results[0].Name}
							_, err = collection.UpdateOne(context.TODO(), filter, update, updateOptions)
							if err != nil {
								zap.L().Error("Failed to update document", zap.Error(err))
							} else {
								zap.L().Info("Successfully updated document", zap.String("company", results[0].Name))
							}
						}
					} else {
						zap.L().Error("No score available for", zap.String("company", instrumentName))
					}

					// Marshal and write the stockDetail
					stockDataMarshal, err := json.Marshal(stockDetail)
					if err != nil {
						zap.L().Error("Error marshalling data", zap.Error(err))
						continue
					}

					_, err = ctx.Writer.Write(append(stockDataMarshal, '\n')) // Send each stockDetail as JSON with a newline separator

					if err != nil {
						zap.L().Error("Error writing data", zap.Error(err))
						break
					}
					ctx.Writer.Flush() // Flush each chunk immediately
				}
			}
		}
	}

	return nil
}