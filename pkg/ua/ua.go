package ua

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/cloudwebrtc/go-sip-ua/pkg/account"
	"github.com/cloudwebrtc/go-sip-ua/pkg/auth"
	"github.com/cloudwebrtc/go-sip-ua/pkg/session"
	"github.com/cloudwebrtc/go-sip-ua/pkg/stack"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/sip/parser"
	"github.com/ghettovoice/gosip/transaction"
	"github.com/ghettovoice/gosip/util"
)

// UserAgentConfig .
type UserAgentConfig struct {
	UserAgent string
	SipStack  *stack.SipStack
	log       log.Logger
}

//InviteSessionHandler .
type InviteSessionHandler func(s *session.Session, req *sip.Request, resp *sip.Response, status session.Status)

//RegisterHandler .
type RegisterHandler func(regState account.RegisterState)

//UserAgent .
type UserAgent struct {
	InviteStateHandler   InviteSessionHandler
	RegisterStateHandler RegisterHandler
	config               *UserAgentConfig
	iss                  sync.Map /*Invite Session*/
	log                  log.Logger
}

//NewUserAgent .
func NewUserAgent(config *UserAgentConfig, logger log.Logger) *UserAgent {
	ua := &UserAgent{
		config:               config,
		iss:                  sync.Map{},
		InviteStateHandler:   nil,
		RegisterStateHandler: nil,
		log:                  logger.WithPrefix("UserAgent"),
	}
	stack := config.SipStack
	stack.OnRequest(sip.INVITE, ua.handleInvite)
	stack.OnRequest(sip.ACK, ua.handleACK)
	stack.OnRequest(sip.BYE, ua.handleBye)
	stack.OnRequest(sip.CANCEL, ua.handleCancel)
	return ua
}

func (ua *UserAgent) Log() log.Logger {
	return ua.log
}

func (ua *UserAgent) handleInviteState(is *session.Session, request *sip.Request, response *sip.Response, state session.Status, tx *sip.Transaction) {
	if request != nil && *request != nil {
		is.StoreRequest(*request)
	}

	if response != nil && *response != nil {
		is.StoreResponse(*response)
	}

	if tx != nil {
		is.StoreTransaction(*tx)
	}

	is.SetState(state)

	if ua.InviteStateHandler != nil {
		ua.InviteStateHandler(is, request, response, state)
	}
}

func (ua *UserAgent) BuildRequest(
	method sip.RequestMethod,
	from *sip.Address,
	to *sip.Address,
	contact *sip.Address,
	target sip.SipUri,
	callID *sip.CallID) (*sip.Request, error) {

	builder := sip.NewRequestBuilder().SetMethod(method).SetFrom(from).SetTo(to).SetRecipient(target.Clone()).AddVia(ua.buildViaHopHeader(target))

	if callID != nil {
		builder.SetCallID(callID)
	}
	builder.SetContact(contact)

	userAgent := sip.UserAgentHeader(ua.config.UserAgent)
	builder.SetUserAgent(&userAgent)

	req, err := builder.Build()
	if err != nil {
		ua.Log().Errorf("err => %v", err)
		return nil, err
	}

	ua.Log().Debugf("buildRequest %s => %v", method, req)
	return &req, nil
}

func buildFrom(target sip.SipUri, user string, displayName string) *sip.Address {
	return &sip.Address{
		DisplayName: sip.String{Str: displayName},
		Uri: &sip.SipUri{
			FUser: sip.String{Str: user},
			FHost: target.Host(),
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: util.RandString(8)}),
	}
}

func buildTo(target sip.SipUri) *sip.Address {
	return &sip.Address{
		Uri: &sip.SipUri{
			FIsEncrypted: target.IsEncrypted(),
			FUser:        target.User(),
			FHost:        target.Host(),
		},
	}
}

func (ua *UserAgent) buildViaHopHeader(target sip.SipUri) *sip.ViaHop {
	protocol := "udp"
	if nt, ok := target.UriParams().Get("transport"); ok {
		protocol = nt.String()
	}
	s := ua.config.SipStack
	netinfo := s.GetNetworkInfo(protocol)

	var host string = netinfo.Host
	if net.ParseIP(target.Host()).IsLoopback() {
		host = "127.0.0.1"
	}

	port := netinfo.Port
	if target.Port() != nil {
		port = target.Port()
	}

	viaHop := &sip.ViaHop{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       protocol,
		Host:            host,
		Port:            port,
		Params:          sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	}
	return viaHop
}

func (ua *UserAgent) buildContact(uri sip.SipUri, instanceID *string) *sip.Address {
	s := ua.config.SipStack
	contact := &sip.Address{
		Uri: &sip.SipUri{
			FHost:      "0.0.0.0",
			FUriParams: uri.FUriParams,
		},
	}

	if instanceID != nil {
		contact.Params = sip.NewParams().Add("+sip.instance", sip.String{Str: *instanceID})
	}

	protocol := "udp"
	if nt, ok := uri.UriParams().Get("transport"); ok {
		protocol = nt.String()
	}

	target := s.GetNetworkInfo(protocol)

	var host string = target.Host
	if net.ParseIP(uri.Host()).IsLoopback() {
		host = "127.0.0.1"
	}

	if contact.Uri.Host() == "0.0.0.0" {
		contact.Uri.SetHost(host)
	}

	if contact.Uri.Port() == nil {
		contact.Uri.SetPort(target.Port)
	}
	return contact
}

func (ua *UserAgent) handleRegisterState(profile *account.Profile, resp sip.Response, err error) {

	if err != nil {
		ua.Log().Errorf("Request [%s] failed, err => %v", sip.REGISTER, err)
		if ua.RegisterStateHandler != nil {
			reqErr := err.(*sip.RequestError)
			regState := account.RegisterState{
				Account:    *profile,
				Response:   nil,
				StatusCode: sip.StatusCode(reqErr.Code),
				Reason:     reqErr.Reason,
				Expiration: 0,
			}
			ua.RegisterStateHandler(regState)
		}
	}
	if resp != nil {
		stateCode := resp.StatusCode()
		ua.Log().Debugf("%s resp %d => %s", sip.REGISTER, stateCode, resp.String())
		if ua.RegisterStateHandler != nil {
			var expires sip.Expires = 0
			hdrs := resp.GetHeaders("Expires")
			if len(hdrs) > 0 {
				expires = *(hdrs[0]).(*sip.Expires)
			}
			regState := account.RegisterState{
				Account:    *profile,
				Response:   resp,
				StatusCode: resp.StatusCode(),
				Reason:     resp.Reason(),
				Expiration: uint32(expires),
			}
			ua.RegisterStateHandler(regState)
		}
	}
}

func (ua *UserAgent) SendRegister(profile *account.Profile, target sip.SipUri, expires uint32) {

	from := buildFrom(target, profile.User, profile.DisplayName)
	contact := ua.buildContact(target, &profile.InstanceID)

	to := buildTo(target)
	request, err := ua.BuildRequest(sip.REGISTER, from, to, contact, target, nil)
	if err != nil {
		ua.Log().Errorf("Register: err = %v", err)
		return
	}
	expiresHeader := sip.Expires(expires)
	(*request).AppendHeader(&expiresHeader)

	var authorizer *auth.ClientAuthorizer = nil
	if profile.Auth != nil {
		authorizer = auth.NewClientAuthorizer(profile.Auth.AuthName, profile.Auth.Password)
	}
	resp, err := ua.RequestWithContext(context.TODO(), *request, authorizer, true)
	ua.handleRegisterState(profile, resp, err)
}

func (ua *UserAgent) Invite(profile *account.Profile, target string, body *string) (*session.Session, error) {

	targetURI, err := parser.ParseSipUri(target)
	if err != nil {
		ua.Log().Error(err)
		return nil, err
	}

	from := buildFrom(targetURI, profile.User, profile.DisplayName)
	contact := ua.buildContact(targetURI, &profile.InstanceID)
	to := buildTo(targetURI)

	request, err := ua.BuildRequest(sip.INVITE, from, to, contact, targetURI, nil)
	if err != nil {
		ua.Log().Errorf("INVITE: err = %v", err)
		return nil, err
	}

	if body != nil {
		(*request).SetBody(*body, true)
		contentType := sip.ContentType("application/sdp")
		(*request).AppendHeader(&contentType)
	}

	var authorizer *auth.ClientAuthorizer = nil
	if profile.Auth != nil {
		authorizer = auth.NewClientAuthorizer(profile.Auth.AuthName, profile.Auth.Password)
	}

	resp, err := ua.RequestWithContext(context.TODO(), *request, authorizer, false)
	if err != nil {
		ua.Log().Errorf("INVITE: Request [INVITE] failed, err => %v", err)
		return nil, err
	}

	if resp != nil {
		stateCode := resp.StatusCode()
		ua.Log().Debugf("INVITE: resp %d => %s", stateCode, resp.String())
		return nil, fmt.Errorf("Invite session is unsuccessful, code: %d, reason: %s", stateCode, resp.String())
	}

	callID, ok := (*request).CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			return v.(*session.Session), nil
		}
	}

	return nil, fmt.Errorf("Invite session not found, unknown errors.")
}

func (ua *UserAgent) Request(req *sip.Request) {
	ua.config.SipStack.Request(*req)
}

func (ua *UserAgent) handleBye(request sip.Request, tx sip.ServerTransaction) {

	ua.Log().Debugf("handleBye: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")

	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*session.Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			ua.handleInviteState(is, &request, nil, session.Terminated, &transaction)
		}
	}

	tx.Respond(response)
}

func (ua *UserAgent) handleCancel(request sip.Request, tx sip.ServerTransaction) {

	ua.Log().Debugf("handleCancel: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")
	tx.Respond(response)

	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*session.Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			is.SetState(session.Canceled)
			ua.handleInviteState(is, &request, nil, session.Canceled, &transaction)
		}
	}
}

func (ua *UserAgent) handleACK(request sip.Request, tx sip.ServerTransaction) {

	ua.Log().Debugf("handleACK => %s, body => %s", request.Short(), request.Body())
	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			// handle Ringing or Processing with sdp
			is := v.(*session.Session)
			is.SetState(session.Confirmed)
			ua.handleInviteState(is, &request, nil, session.Confirmed, nil)
		}
	}
}

func (ua *UserAgent) handleInvite(request sip.Request, tx sip.ServerTransaction) {

	ua.Log().Debugf("handleInvite => %s, body => %s", request.Short(), request.Body())

	callID, ok := request.CallID()
	if ok {
		var transaction sip.Transaction = tx.(sip.Transaction)
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*session.Session)
			is.SetState(session.ReInviteReceived)
			ua.handleInviteState(is, &request, nil, session.ReInviteReceived, &transaction)
		} else {
			contact, _ := request.Contact()
			is := session.NewInviteSession(ua.RequestWithContext, "UAS", contact, request, *callID, transaction, session.Incoming, ua.Log())
			ua.iss.Store(*callID, is)
			is.SetState(session.InviteReceived)
			ua.handleInviteState(is, &request, nil, session.InviteReceived, &transaction)
			is.SetState(session.WaitingForAnswer)
		}
	}

	go func() {
		cancel := <-tx.Cancels()
		if cancel != nil {
			ua.Log().Debugf("Cancel => %s, body => %s", cancel.Short(), cancel.Body())
			response := sip.NewResponseFromRequest(cancel.MessageID(), cancel, 200, "OK", "")
			if callID, ok := response.CallID(); ok {
				if v, found := ua.iss.Load(*callID); found {
					ua.iss.Delete(*callID)
					is := v.(*session.Session)
					is.SetState(session.Canceled)
					ua.handleInviteState(is, &request, &response, session.Canceled, nil)
				}
			}

			tx.Respond(response)
		}
	}()

	go func() {
		ack := <-tx.Acks()
		if ack != nil {
			ua.Log().Debugf("ack => %v", ack)
		}
	}()
}

// RequestWithContext .
func (ua *UserAgent) RequestWithContext(ctx context.Context, request sip.Request, authorizer sip.Authorizer, waitForResult bool) (sip.Response, error) {
	s := ua.config.SipStack
	tx, err := s.Request(request)
	if err != nil {
		return nil, err
	}

	if request.IsInvite() {
		callID, ok := request.CallID()
		if ok {
			var transaction sip.Transaction = tx.(sip.Transaction)
			if _, found := ua.iss.Load(*callID); !found {
				contact, _ := request.Contact()
				is := session.NewInviteSession(ua.RequestWithContext, "UAC", contact, request, *callID, transaction, session.Outgoing, ua.Log())
				ua.iss.Store(*callID, is)
				is.ProvideOffer(request.Body())
				is.SetState(session.InviteSent)
				ua.handleInviteState(is, &request, nil, session.InviteSent, &transaction)
			}
		}
	}

	responses := make(chan sip.Response)
	provisionals := make(chan sip.Response)
	errs := make(chan error)
	go func() {
		var lastResponse sip.Response

		previousResponses := make([]sip.Response, 0)
		previousResponsesStatuses := make(map[sip.StatusCode]bool)

		for {
			select {
			case <-ctx.Done():
				if lastResponse != nil && lastResponse.IsProvisional() {
					s.CancelRequest(request, lastResponse)
				}
				if lastResponse != nil {
					lastResponse.SetPrevious(previousResponses)
				}
				errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
				// pull out later possible transaction responses and errors
				go func() {
					for {
						select {
						case <-tx.Done():
							return
						case <-tx.Errors():
						case <-tx.Responses():
						}
					}
				}()
				return
			case err, ok := <-tx.Errors():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				}

				switch err.(type) {
				case *transaction.TxTimeoutError:
					{
						errs <- sip.NewRequestError(408, "Request Timeout", request, lastResponse)
						return
					}
				}

				//errs <- err
				return
			case response, ok := <-tx.Responses():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				}

				response = sip.CopyResponse(response)
				lastResponse = response

				if response.IsProvisional() {
					if _, ok := previousResponsesStatuses[response.StatusCode()]; !ok {
						previousResponses = append(previousResponses, response)
					}
					provisionals <- response
					continue
				}

				// success
				if response.IsSuccess() {
					response.SetPrevious(previousResponses)

					if request.IsInvite() {
						s.AckInviteRequest(request, response)
						s.RememberInviteRequest(request)
						go func() {
							for response := range tx.Responses() {
								s.AckInviteRequest(request, response)
							}
						}()
					}
					responses <- response
					tx.Done()
					return
				}

				// unauth request
				if (response.StatusCode() == 401 || response.StatusCode() == 407) && authorizer != nil {
					if err := authorizer.AuthorizeRequest(request, response); err != nil {
						errs <- err
						return
					}
					if response, err := ua.RequestWithContext(ctx, request, nil, true); err == nil {
						responses <- response
					} else {
						errs <- err
					}
					return
				}

				// failed request
				if lastResponse != nil {
					lastResponse.SetPrevious(previousResponses)
				}
				errs <- sip.NewRequestError(uint(response.StatusCode()), response.Reason(), request, lastResponse)
				return
			}
		}
	}()

	waitForResponse := func() (sip.Response, error) {
		for {
			select {
			case provisional := <-provisionals:
				callID, ok := provisional.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*session.Session)
						is.StoreResponse(provisional)
						// handle Ringing or Processing with sdp
						ua.handleInviteState(is, &request, &provisional, session.Provisional, nil)
						if len(provisional.Body()) > 0 {
							is.SetState(session.EarlyMedia)
							ua.handleInviteState(is, &request, &provisional, session.EarlyMedia, nil)
						}
					}
				}
			case err := <-errs:
				//TODO: error type switch transaction.TxTimeoutError
				switch err.(type) {
				case *transaction.TxTimeoutError:
					//errs <- sip.NewRequestError(408, "Request Timeout", nil, nil)
					return nil, err
				}
				request := (err.(*sip.RequestError)).Request
				response := (err.(*sip.RequestError)).Response
				callID, ok := request.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*session.Session)
						ua.iss.Delete(*callID)
						// handle Ringing or Processing with sdp
						is.SetState(session.Failure)
						ua.handleInviteState(is, &request, &response, session.Failure, nil)
					}
				}
				return nil, err
			case response := <-responses:
				callID, ok := response.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						if request.IsInvite() {
							// handle Ringing or Processing with sdp
							is := v.(*session.Session)
							is.SetState(session.Confirmed)
							ua.handleInviteState(is, &request, &response, session.Confirmed, nil)
						}
					}
				}
				return response, nil
			}
		}
	}

	if !waitForResult {
		go waitForResponse()
		return nil, err
	}
	return waitForResponse()
}

func (ua *UserAgent) Shutdown() {
	ua.config.SipStack.Shutdown()
}
