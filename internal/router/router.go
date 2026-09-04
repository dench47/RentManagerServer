package router

import (
	"log"
	"os"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/handler"
	"rentmanager-server/internal/middleware"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
)

func Setup(cfg *config.Config) *gin.Engine {
	r := gin.Default()
	_ = r.SetTrustedProxies([]string{"127.0.0.1"})

	// CORS
	r.Use(middleware.CORSMiddleware())

	// Static files
	r.Static("/uploads", cfg.UploadDir)

	// Init handlers
	callCheckSvc := service.NewCallCheckService(getEnvDefault("CALLCHECK_API_ID", "stub"))
	s3Svc := service.NewS3Service(cfg.S3)
	telegramSvc := service.NewTelegramService(cfg.Telegram)
	emailSvc := service.NewEmailService(cfg.Email)

	// FCM
	var fcmSvc *service.FCMService
	fcmCredsPath := os.Getenv("FCM_CREDENTIALS_PATH")
	if fcmCredsPath != "" {
		data, err := os.ReadFile(fcmCredsPath)
		if err != nil {
			log.Printf("WARNING: FCM disabled — cannot read credentials %s: %v", fcmCredsPath, err)
		} else {
			fcmSvc, err = service.NewFCMService(data)
			if err != nil {
				panic("FCM init failed: " + err.Error())
			}
		}
	}

	authHandler := handler.NewAuthHandler(nil, callCheckSvc, s3Svc, fcmSvc, telegramSvc, emailSvc, cfg.JWT, cfg.UploadDir)
	propertyHandler := handler.NewPropertyHandler(s3Svc)
	meterHandler := handler.NewMeterHandler()
	tenantHandler := handler.NewTenantHandler(fcmSvc)
	paymentHandler := handler.NewPaymentHandler()
	userHandler := handler.NewUserHandler()
	bookingHandler := handler.NewBookingHandler()
	financeHandler := handler.NewFinanceHandler()
	chatHandler := handler.NewChatHandler()
	uploadHandler := handler.NewUploadHandler(cfg.UploadDir, s3Svc)
	versionHandler := handler.NewVersionHandler(cfg.DownloadDir+"/version.json", cfg.APKBaseURL)

	// Static downloads (APK)
	r.Static("/downloads", cfg.DownloadDir)

	api := r.Group("/api/v1")

	// Public version endpoint
	api.GET("/version", versionHandler.GetVersion)

	// Telegram webhook (public)
	api.POST("/telegram/webhook", authHandler.TelegramWebhook)

	// Public auth
	auth := api.Group("/auth")
	{
		auth.POST("/login", authHandler.Login)
		auth.POST("/callcheck/add", authHandler.CallCheckAdd)
		auth.POST("/callcheck/status", authHandler.CallCheckStatus)
		auth.POST("/refresh", authHandler.RefreshToken)
		auth.POST("/verify_password", authHandler.VerifyPassword)
		auth.GET("/pin_attempts", authHandler.PinAttempts)

		// Подтверждение входа с нового устройства (Device Trust)
		auth.POST("/login/request_approval", authHandler.RequestLoginApproval)
		auth.GET("/login/status", authHandler.LoginStatus)

		// Вход через Telegram (код в мессенджер)
		auth.POST("/login/telegram_code", authHandler.TelegramSendCode)
		auth.POST("/login/telegram_verify", authHandler.TelegramVerifyCode)

		// Вход через Email (код на подтверждённую почту)
		auth.POST("/login/email_code", authHandler.EmailLoginSendCode)
		auth.POST("/login/email_verify", authHandler.EmailLoginVerifyCode)
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
		protected.POST("/auth/unregister_device", authHandler.UnregisterDevice)
		protected.DELETE("/auth/account", authHandler.DeleteAccount)

		// Подтверждение входа: действия доверенного устройства
		protected.POST("/auth/login/approve", authHandler.ApproveLogin)
		protected.POST("/auth/login/deny", authHandler.DenyLogin)

		// Управление доверенными устройствами
		protected.GET("/auth/devices", authHandler.ListDevices)
		protected.DELETE("/auth/devices/:deviceId", authHandler.RevokeDevice)

		// Telegram привязка
		protected.POST("/auth/telegram/link", authHandler.TelegramLink)
		protected.GET("/auth/telegram/status", authHandler.TelegramStatus)
		protected.POST("/auth/telegram/unlink", authHandler.TelegramUnlink)

		// Email подтверждение
		protected.POST("/auth/email/send_code", authHandler.EmailSendCode)
		protected.POST("/auth/email/verify", authHandler.EmailVerify)
		protected.GET("/auth/email/status", authHandler.EmailStatus)

		// Users
		protected.GET("/users/me", authHandler.GetMe)
		protected.GET("/users/search", userHandler.Search)

		// Properties
		protected.GET("/properties", propertyHandler.List)
		protected.POST("/properties", propertyHandler.Create)
		protected.GET("/properties/:id", propertyHandler.Get)
		protected.PUT("/properties/:id", propertyHandler.Update)
		protected.DELETE("/properties/:id", propertyHandler.Delete)
		protected.POST("/properties/:id/publish", propertyHandler.Publish)
		protected.POST("/properties/:id/unpublish", propertyHandler.Unpublish)
		protected.POST("/properties/:id/photos", propertyHandler.AddPhoto)
		protected.DELETE("/photos/:photoId", propertyHandler.DeletePhoto)

		// Attach tenant
		protected.POST("/properties/:id/attach_tenant", tenantHandler.AttachToProperty)

		// Bookings (занятость объекта — заливка шахматки)
		protected.GET("/properties/:id/bookings", bookingHandler.List)
		protected.POST("/properties/:id/bookings", bookingHandler.Create)
		protected.PUT("/properties/:id/bookings/:bookingId", bookingHandler.Update)
		protected.DELETE("/properties/:id/bookings/:bookingId", bookingHandler.Delete)

		// Tenant view: арендованные объекты и арендодатели
		protected.GET("/tenant/properties", propertyHandler.ListForTenant)
		protected.GET("/tenant/landlords", userHandler.ListLandlordsForTenant)

		// Meters
		protected.GET("/properties/:id/meters", meterHandler.ListByProperty)
		protected.POST("/properties/:id/meters", meterHandler.Create)
		protected.PUT("/meters/:meterId", meterHandler.Update)
		protected.DELETE("/meters/:meterId", meterHandler.Delete)
		protected.GET("/meters/:meterId/readings", meterHandler.ListReadings)
		protected.POST("/meters/:meterId/readings", meterHandler.CreateReading)

		// Tenants
		protected.GET("/tenants", tenantHandler.List)
		protected.GET("/tenants/:id", tenantHandler.Get)
		protected.POST("/tenants", tenantHandler.Create)
		protected.DELETE("/tenants/:id", tenantHandler.Delete)

		// Payment schedules
		protected.GET("/payments/schedule", paymentHandler.ListSchedules)
		protected.POST("/payments/schedule", paymentHandler.CreateSchedule)
		protected.GET("/tenant/schedules", paymentHandler.ListSchedulesForTenant)

		// Payments (имитация оплаты)
		protected.GET("/properties/:id/payments", paymentHandler.ListPayments)
		protected.POST("/properties/:id/payments", paymentHandler.CreatePayment)

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
