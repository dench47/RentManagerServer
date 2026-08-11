package router

import (
	"os"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/handler"
	"rentmanager-server/internal/middleware"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
)

func Setup(cfg *config.Config) *gin.Engine {
	r := gin.Default()

	// CORS
	r.Use(middleware.CORSMiddleware())

	// Static files
	r.Static("/uploads", cfg.UploadDir)

	// Init handlers
	callCheckSvc := service.NewCallCheckService(getEnvDefault("CALLCHECK_API_ID", "stub"))
	s3Svc := service.NewS3Service(cfg.S3)

	// FCM
	var fcmSvc *service.FCMService
	fcmCredsPath := os.Getenv("FCM_CREDENTIALS_PATH")
	if fcmCredsPath != "" {
		data, err := os.ReadFile(fcmCredsPath)
		if err == nil {
			fcmSvc, err = service.NewFCMService(data)
			if err != nil {
				panic("FCM init failed: " + err.Error())
			}
		}
	}

	authHandler := handler.NewAuthHandler(nil, callCheckSvc, s3Svc, fcmSvc, cfg.JWT)
	propertyHandler := handler.NewPropertyHandler()
	meterHandler := handler.NewMeterHandler()
	tenantHandler := handler.NewTenantHandler()
	paymentHandler := handler.NewPaymentHandler()
	financeHandler := handler.NewFinanceHandler()
	chatHandler := handler.NewChatHandler()
	uploadHandler := handler.NewUploadHandler(cfg.UploadDir, s3Svc)
	versionHandler := handler.NewVersionHandler(cfg.DownloadDir+"/version.json", cfg.APKBaseURL)

	// Static downloads (APK)
	r.Static("/downloads", cfg.DownloadDir)

	api := r.Group("/api/v1")

	// Public version endpoint
	api.GET("/version", versionHandler.GetVersion)

	// Public auth
	auth := api.Group("/auth")
	{
		auth.POST("/login", authHandler.Login)
		auth.POST("/callcheck/add", authHandler.CallCheckAdd)
		auth.POST("/callcheck/status", authHandler.CallCheckStatus)
		auth.POST("/refresh", authHandler.RefreshToken)
		auth.POST("/verify_password", authHandler.VerifyPassword)
	}

	// Protected routes
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(cfg.JWT))
	{
		protected.POST("/auth/save_name", authHandler.SaveName)
		protected.PUT("/auth/profile", authHandler.UpdateProfile)
		protected.POST("/auth/change_phone", authHandler.ChangePhone)
		protected.POST("/auth/confirm_phone_change", authHandler.ConfirmPhoneChange)
		protected.POST("/auth/set_password", authHandler.SetPassword)
		protected.POST("/auth/logout_all", authHandler.LogoutAll)
		protected.POST("/auth/register_device", authHandler.RegisterDevice)
		protected.DELETE("/auth/account", authHandler.DeleteAccount)

		// Users
		protected.GET("/users/me", authHandler.GetMe)

		// Properties
		protected.GET("/properties", propertyHandler.List)
		protected.POST("/properties", propertyHandler.Create)
		protected.GET("/properties/:id", propertyHandler.Get)
		protected.PUT("/properties/:id", propertyHandler.Update)
		protected.DELETE("/properties/:id", propertyHandler.Delete)

		// Attach tenant
		protected.POST("/properties/:id/attach_tenant", tenantHandler.AttachToProperty)

		// Meters
		protected.GET("/properties/:id/meters", meterHandler.ListByProperty)
		protected.POST("/properties/:id/meters", meterHandler.Create)
		protected.PUT("/meters/:meterId", meterHandler.Update)
		protected.DELETE("/meters/:meterId", meterHandler.Delete)

		// Tenants
		protected.GET("/tenants", tenantHandler.List)
		protected.GET("/tenants/:id", tenantHandler.Get)
		protected.POST("/tenants", tenantHandler.Create)
		protected.DELETE("/tenants/:id", tenantHandler.Delete)

		// Payment schedules
		protected.GET("/payments/schedule", paymentHandler.ListSchedules)
		protected.POST("/payments/schedule", paymentHandler.CreateSchedule)

		// Finance
		protected.GET("/finance/report", financeHandler.Report)

		// Chats
		protected.GET("/chats", chatHandler.ListChats)
		protected.GET("/chats/:chatId/messages", chatHandler.GetMessages)
		protected.POST("/chats/:chatId/messages", chatHandler.SendMessage)

		// Upload
		protected.POST("/upload", uploadHandler.Upload)
	}

	return r
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
