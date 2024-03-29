package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"one-api/common"
	"one-api/relay/constant"
	"strings"

	"github.com/gin-gonic/gin"
)

func StreamHandler(c *gin.Context, resp *http.Response, relayMode int) (*ErrorWithStatusCode, string) {
	responseText := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := strings.Index(string(data), "\n"); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	dataChan := make(chan string)
	stopChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			data := scanner.Text()
			if len(data) < 6 { // ignore blank line or wrong format
				continue
			}
			if data[:6] != "data: " && data[:6] != "[DONE]" {
				continue
			}
			dataChan <- data
			data = data[6:]
			if !strings.HasPrefix(data, "[DONE]") {
				switch relayMode {
				case constant.RelayModeChatCompletions:
					var streamResponse ChatCompletionsStreamResponse
					err := json.Unmarshal([]byte(data), &streamResponse)
					if err != nil {
						common.SysError("error unmarshalling stream response: " + err.Error())
						continue // just ignore the error
					}
					for _, choice := range streamResponse.Choices {
						responseText += choice.Delta.Content
					}
				case constant.RelayModeCompletions:
					var streamResponse CompletionsStreamResponse
					err := json.Unmarshal([]byte(data), &streamResponse)
					if err != nil {
						common.SysError("error unmarshalling stream response: " + err.Error())
						continue
					}
					for _, choice := range streamResponse.Choices {
						responseText += choice.Text
					}
				}
			}
		}
		stopChan <- true
	}()
	common.SetEventStreamHeaders(c)
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			if strings.HasPrefix(data, "data: [DONE]") {
				data = data[:12]
			}
			// some implementations may add \r at the end of data
			data = strings.TrimSuffix(data, "\r")
			c.Render(-1, common.CustomEvent{Data: data})
			return true
		case <-stopChan:
			return false
		}
	})
	err := resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), ""
	}
	return nil, responseText
}

func Handler(c *gin.Context, resp *http.Response, promptTokens int, model string) (*ErrorWithStatusCode, *Usage, string) {
	var textResponse SlimTextResponse
	var responseText string
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil, ""
	}
	err = resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil, ""
	}
	err = json.Unmarshal(responseBody, &textResponse)
	if err != nil {
		return ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil, ""
	}
	if textResponse.Error.Type != "" {
		return &ErrorWithStatusCode{
			Error:      textResponse.Error,
			StatusCode: resp.StatusCode,
		}, nil, ""
	}
	// Reset response body
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil, ""
	}
	err = resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil, ""
	}
	for _, choice := range textResponse.Choices {
		responseText = choice.Message.StringContent()
	}
	if textResponse.Usage.TotalTokens == 0 {
		completionTokens := 0
		completionTokens += CountTokenText(responseText, model)
		textResponse.Usage = Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	return nil, &textResponse.Usage, responseText
}
