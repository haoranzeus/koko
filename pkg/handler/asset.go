package handler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jumpserver/koko/pkg/i18n"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/model"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/service"
	"github.com/jumpserver/koko/pkg/logger"
	"github.com/jumpserver/koko/pkg/proxy"
	"github.com/jumpserver/koko/pkg/srvconn"
	"github.com/jumpserver/koko/pkg/utils"
)

func (u *UserSelectHandler) retrieveRemoteAsset(reqParam model.PaginationParam) []model.Asset {
	res, err := u.h.jmsService.GetUserPermsAssets(u.user.ID, reqParam)
	if err != nil {
		logger.Errorf("Get user perm assets failed: %s", err.Error())
	}
	return u.updateRemotePageData(reqParam, res)
}

func (u *UserSelectHandler) searchLocalAsset(searches ...string) []model.Asset {
	fields := map[string]struct{}{
		"name":     {},
		"address":  {},
		"ip":       {},
		"platform": {},
		"org_name": {},
		"comment":  {},
	}
	return u.searchLocalFromFields(fields, searches...)
}

func (u *UserSelectHandler) displayAssetResult(searchHeader string) {
	lang := i18n.NewLang(u.h.i18nLang)
	if len(u.currentResult) == 0 {
		noAssets := lang.T("No Assets")
		u.displayNoResultMsg(searchHeader, noAssets)
		return
	}
	u.displayAssets(searchHeader)
}

func (u *UserSelectHandler) displayAssets(searchHeader string) {
	lang := i18n.NewLang(u.h.i18nLang)
	idLabel := lang.T("ID")
	nameLabel := lang.T("Name")
	ipLabel := lang.T("Address")
	protocolsLabel := lang.T("Protocols")
	platformLabel := lang.T("Platform")
	orgLabel := lang.T("Organization")
	commentLabel := lang.T("Comment")

	Labels := []string{idLabel, nameLabel, ipLabel, protocolsLabel, platformLabel, orgLabel, commentLabel}
	fields := []string{"ID", "Name", "Address", "Protocols", "Platform", "Organization", "Comment"}
	fieldsSize := map[string][3]int{
		"ID":           {0, 0, 5},
		"Name":         {0, 8, 0},
		"Address":      {0, 8, 40},
		"Protocols":    {0, 8, 0},
		"Platform":     {0, 8, 0},
		"Organization": {0, 8, 0},
		"Comment":      {0, 0, 0},
	}
	generateRowFunc := func(i int, item *model.Asset) map[string]string {
		row := make(map[string]string)
		row["ID"] = strconv.Itoa(i + 1)
		row["Name"] = item.Name
		row["Address"] = item.Address
		row["Protocols"] = strings.Join(item.SupportProtocols(), "|")
		row["Platform"] = item.Platform.Name
		row["Organization"] = item.OrgName
		row["Comment"] = joinMultiLineString(item.Comment)
		return row
	}
	assetDisplay := lang.T("the asset")
	data := make([]map[string]string, len(u.currentResult))
	for i := range u.currentResult {
		data[i] = generateRowFunc(i, &u.currentResult[i])
	}
	u.displayResult(searchHeader, assetDisplay,
		Labels, fields, fieldsSize, generateRowFunc)

}

func (u *UserSelectHandler) proxyAsset(asset model.Asset) {
	u.selectedAsset = &asset
	accounts, err := u.h.jmsService.GetAccountsByUserIdAndAssetId(u.user.ID, asset.ID)
	if err != nil {
		logger.Errorf("Get asset accounts err: %s", err)
		return
	}
	protocol, ok := u.h.chooseAssetProtocol(asset.SupportProtocols())
	if !ok {
		logger.Info("not select protocol")
		return
	}
	i18nLang := u.h.i18nLang
	lang := i18n.NewLang(i18nLang)
	if err2 := srvconn.IsSupportedProtocol(protocol); err2 != nil {
		var errMsg string
		switch {
		case errors.As(err2, &srvconn.ErrNoClient{}):
			errMsg = lang.T("%s protocol client not installed.")
			errMsg = fmt.Sprintf(errMsg, protocol)
		default:
			errMsg = lang.T("Terminal does not support protocol %s, please use web terminal to access")
			errMsg = fmt.Sprintf(errMsg, protocol)
		}
		utils.IgnoreErrWriteString(u.h.term, utils.WrapperWarn(errMsg))
		return
	}

	selectedAccount, ok := u.h.chooseAccount(accounts)
	if !ok {
		return
	}
	u.selectedAccount = &selectedAccount
	req := service.SuperConnectTokenReq{
		UserId:        u.user.ID,
		AssetId:       asset.ID,
		Account:       selectedAccount.Alias,
		Protocol:      protocol,
		ConnectMethod: "ssh",
	}
	tokenInfo, err := u.h.jmsService.CreateSuperConnectToken(&req)
	if err != nil {
		if tokenInfo.Code == "" {
			logger.Errorf("Create connect token and auth info failed: %s", err)
			utils.IgnoreErrWriteString(u.h.term, lang.T("Core API failed"))
			return
		}
		switch tokenInfo.Code {
		case model.ACLReject:
			logger.Errorf("Create connect token and auth info failed: %s", tokenInfo.Detail)
			utils.IgnoreErrWriteString(u.h.term, lang.T("ACL reject"))
			utils.IgnoreErrWriteString(u.h.term, utils.CharNewLine)
			return
		case model.ACLReview:
			reviewHandler := LoginReviewHandler{
				readWriter: u.h.sess,
				i18nLang:   u.h.i18nLang,
				user:       u.user,
				jmsService: u.h.jmsService,
				req:        &req,
			}
			ok2, err2 := reviewHandler.WaitReview(u.h.sess.Context())
			if err2 != nil {
				logger.Errorf("Wait login review failed: %s", err)
				utils.IgnoreErrWriteString(u.h.term, lang.T("Core API failed"))
				return
			}
			if !ok2 {
				logger.Error("Wait login review failed")
				return
			}
			tokenInfo = reviewHandler.tokenInfo
		default:
			logger.Errorf("Create connect token and auth info failed: %s %s", tokenInfo.Code, tokenInfo.Detail)
			return
		}
	}

	connectToken, err := u.h.jmsService.GetConnectTokenInfo(tokenInfo.ID)
	if err != nil {
		logger.Errorf("connect token err: %s", err)
		utils.IgnoreErrWriteString(u.h.term, lang.T("get connect token err"))
		return
	}
	proxyOpts := make([]proxy.ConnectionOption, 0, 10)
	proxyOpts = append(proxyOpts, proxy.ConnectTokenAuthInfo(&connectToken))
	proxyOpts = append(proxyOpts, proxy.ConnectI18nLang(i18nLang))
	srv, err := proxy.NewServer(u.h.sess, u.h.jmsService, proxyOpts...)
	if err != nil {
		logger.Errorf("create proxy server err: %s", err)
		return
	}
	srv.Proxy()
}
