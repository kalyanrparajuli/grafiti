// Copyright © 2017 grafiti/predator authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudtrail"
	"github.com/aws/aws-sdk-go/service/cloudtrail/cloudtrailiface"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	jq "github.com/threatgrid/jqpipe-go"
	"github.com/tidwall/gjson"

	"github.com/coreos/grafiti/arn"
)

var inputFile string

// Holds data that identifies a raw CloudTrail event: gjson.Result search path
// for resource name, and resource type
type rawEventIdentity struct {
	ResourceType     string
	ResourceNamePath string
}

// Maps CloudTrail eventName to a rawEventIdentity
var rawEventMap = map[string]rawEventIdentity{
	"RunInstances":           {arn.EC2InstanceRType, "responseElements.instancesSet.items.0.instanceId"},
	"CreateBucket":           {arn.S3BucketRType, "requestParameters.bucketName"},
	"CreateAutoScalingGroup": {arn.AutoScalingGroupRType, "requestParameters.autoScalingGroupName"},
	"CreateVpc":              {arn.EC2VPCRType, "responseElements.vpc.vpcId"},
	"CreateSubnet":           {arn.EC2SubnetRType, "responseElements.subnet.subnetId"},
	"CreateLoadBalancer":     {arn.ElasticLoadBalancingLoadBalancerRType, "requestParameters.loadBalancerName"},
	"CreateInternetGateway":  {arn.EC2InternetGatewayRType, "responseElements.internetGateway.internetGatewayId"},
	"CreateSecurityGroup":    {arn.EC2SecurityGroupRType, "responseElements.groupId"},
	"CreateNetworkInterface": {arn.EC2NetworkInterfaceRType, "responseElements.networkInterface.networkInterfaceId"},
}

func init() {
	RootCmd.AddCommand(parseCmd)
	parseCmd.PersistentFlags().StringVarP(&inputFile, "input-file", "f", "", "CloudTrail log file of raw CloudTrail events.")
}

var parseCmd = &cobra.Command{
	Use:   "parse",
	Short: "Parse and output resources by reading CloudTrail logs",
	Long:  "Parse a CloudTrail Log and output resources. By default, grafiti talks to the configured AWS account and reads directly from the CloudTrail API.",
	RunE:  runParseCommand,
}

func runParseCommand(cmd *cobra.Command, args []string) error {
	if inputFile != "" {
		return parseFromFile(inputFile)
	}

	sess := session.Must(session.NewSession(
		&aws.Config{
			Region: aws.String(viper.GetString("region")),
		},
	))
	if err := parseFromCloudTrail(cloudtrail.New(sess)); err != nil {
		return err
	}

	return nil
}

// CloudTrailLogFile holds the array of Record strings in a S3 CloudTrail log
// archive.
type CloudTrailLogFile struct {
	Events []json.RawMessage `json:"Records"`
}

func parseFromFile(logFileName string) error {
	raw, err := ioutil.ReadFile(logFileName)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	var logFile CloudTrailLogFile
	if err = json.Unmarshal(raw, &logFile); err != nil {
		return err
	}

	var event []byte
	for _, eventData := range logFile.Events {
		event, err = eventData.MarshalJSON()
		if err != nil {
			continue
		}

		fmt.Println(parseRawCloudTrailEvent(string(event)))
	}

	return nil
}

func parseRawCloudTrailEvent(event string) string {
	parsedEvent := gjson.Parse(event)
	eventName := parsedEvent.Get("eventName")
	eventIdentity, ok := rawEventMap[eventName.String()]
	if !ok {
		return ""
	}

	rn := arn.ResourceName(parsedEvent.Get(eventIdentity.ResourceNamePath).String())
	rt := arn.ResourceType(eventIdentity.ResourceType)

	return parseDataFromEvent(rt, rn, parsedEvent, nil)
}

// NotTaggedFilter holds the resource types of all resources not tagged
type NotTaggedFilter struct {
	Type string `json:"type"`
}

func parseFromCloudTrail(svc cloudtrailiface.CloudTrailAPI) error {
	var start, end *time.Time
	// Check if timestamps or hours exist
	if viper.IsSet("startTimeStamp") && viper.IsSet("endTimeStamp") {
		start, end = calcTimeWindowFromTimeStamp(viper.GetString("startTimeStamp"), viper.GetString("endTimeStamp"))
	} else if viper.IsSet("startHour") && viper.IsSet("endHour") {
		start, end = calcTimeWindowFromHourRange(viper.GetInt("startHour"), viper.GetInt("endHour"))
	}
	if start == nil || end == nil {
		return nil
	}

	// Create LookupEvents for all grafiti.resourceTypes. If none are specified,
	// look up all events for all resourceTypes
	rts := viper.GetStringSlice("resourceTypes")
	var attrs []*cloudtrail.LookupAttribute
	if len(rts) == 0 {
		attrs = []*cloudtrail.LookupAttribute{nil}
	} else {
		for _, rt := range rts {
			attrs = append(attrs, &cloudtrail.LookupAttribute{
				AttributeKey:   aws.String("ResourceType"),
				AttributeValue: aws.String(rt),
			})
		}
	}

	for _, attr := range attrs {
		if err := parseLookupAttribute(svc, attr, start, end); err != nil {
			return err
		}
	}

	return nil
}

// Calculates a time window between a starting RFC3339 timestamp string and
// ending RFC3339 timestamp string.
func calcTimeWindowFromTimeStamp(start, end string) (*time.Time, *time.Time) {
	startTime, err := time.Parse(time.RFC3339, start)
	if err != nil {
		fmt.Println("startTimeStamp parse error:", err.Error())
		return nil, nil
	}

	endTime, err := time.Parse(time.RFC3339, end)
	if err != nil {
		fmt.Println("endTimeStamp parse error:", err.Error())
		return nil, nil
	}

	if startTime.After(endTime) || startTime.Equal(endTime) {
		fmt.Printf(`{"error": "startTimeStamp (%s) is at or after endTimeStamp (%s)"}%s`, startTime, endTime, "\n")
		return nil, nil
	}

	return aws.Time(startTime), aws.Time(endTime)
}

// Calculates a time window between a starting hour and ending hour.
func calcTimeWindowFromHourRange(start, end int) (*time.Time, *time.Time) {
	if start >= end {
		fmt.Printf(`{"error": "startHour (%d) is at or after endHour (%d)"}%s`, start, end, "\n")
		return nil, nil
	}

	now := time.Now()
	startTime := now.Add(time.Duration(start) * time.Hour)
	endTime := now.Add(time.Duration(end) * time.Hour)

	return aws.Time(startTime), aws.Time(endTime)
}

func parseLookupAttribute(svc cloudtrailiface.CloudTrailAPI, attr *cloudtrail.LookupAttribute, start, end *time.Time) error {
	params := &cloudtrail.LookupEventsInput{
		EndTime:          end,
		MaxResults:       aws.Int64(50),
		StartTime:        start,
		LookupAttributes: []*cloudtrail.LookupAttribute{attr},
	}

	for {
		ctx := aws.BackgroundContext()
		resp, err := svc.LookupEventsWithContext(ctx, params)
		if err != nil {
			return err
		}

		printEvents(resp.Events)

		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}

		params.NextToken = resp.NextToken
	}

	return nil
}

// OutputWithEvent holds all data associated with a resource when the
// 'includeEvent' TOML field is set to 'true'
type OutputWithEvent struct {
	Event           *cloudtrail.Event
	TaggingMetadata *TaggingMetadata
	Tags            map[string]string
}

// Output holds all data associated with a resource when the 'includeEvent' TOML
// field is set to 'false'
type Output struct {
	TaggingMetadata *TaggingMetadata
	Tags            map[string]string
}

func printEvents(events []*cloudtrail.Event) {
	for _, e := range events {
		parsedEvent := gjson.Parse(*e.CloudTrailEvent)
		printEvent(e, parsedEvent)
	}
}

func printEvent(event *cloudtrail.Event, parsedEvent gjson.Result) {
	for _, r := range event.Resources {
		nameStr, typeStr := aws.StringValue(r.ResourceName), aws.StringValue(r.ResourceType)

		if nameStr == "" || typeStr == "" {
			continue
		}

		rt, rn := arn.ResourceType(typeStr), arn.ResourceName(nameStr)
		tmString := parseDataFromEvent(rt, rn, parsedEvent, event)
		if tmString != "" {
			fmt.Println(tmString)
		}
	}
}

func parseDataFromEvent(rt arn.ResourceType, rn arn.ResourceName, parsedEvent gjson.Result, event *cloudtrail.Event) string {
	includeEvent := viper.GetBool("includeEvent")
	ARN := arn.MapResourceTypeToARN(rt, rn, parsedEvent)
	if ARN == "" {
		return ""
	}

	tags := getTags(parsedEvent.String())
	tm := &TaggingMetadata{
		ResourceName: rn,
		ResourceType: rt,
		ResourceARN:  ARN,
		CreatorARN:   arn.ResourceARN(parsedEvent.Get("userIdentity.arn").String()),
		CreatorName:  arn.ResourceName(parsedEvent.Get("userIdentity.userName").String()),
	}

	output := getOutput(includeEvent, tags, tm, event)

	oj, err := json.Marshal(output)
	if err != nil {
		fmt.Printf("{\"error\": \"%s\"}\n", err.Error())
	}
	if jsonMatch := matchFilter(oj); jsonMatch {
		return string(oj)
	}

	return ""
}

func matchFilter(output []byte) bool {
	for _, f := range viper.GetStringSlice("filterPatterns") {
		results, err := jq.Eval(string(output), f)
		if err != nil || len(results) == 0 {
			return false
		}

		if rj, _ := results[0].MarshalJSON(); string(rj) != "true" {
			return false
		}
	}

	return true
}

func getTags(rawEvent string) map[string]string {
	tagPatterns := viper.GetStringSlice("tagPatterns")
	if len(tagPatterns) == 0 {
		return map[string]string{}
	}

	allTags := make(map[string]string)
	for _, p := range tagPatterns {
		results, err := jq.Eval(rawEvent, p)
		if err != nil {
			fmt.Printf("{\"error\": \"%s\"}\n", err.Error())
			return nil
		}

		for _, r := range results {
			rBytes, err := r.MarshalJSON()
			if err != nil {
				break
			}

			tagMap, ok := gjson.Parse(string(rBytes)).Value().(map[string]interface{})
			if !ok {
				break
			}

			for k, v := range tagMap {
				if v == nil {
					allTags[k] = ""
				} else {
					allTags[k] = v.(string)
				}
			}
		}
	}
	return allTags
}

func getOutput(includeEvent bool, tags map[string]string, taggingMetadata *TaggingMetadata, event *cloudtrail.Event) interface{} {
	if includeEvent && event != nil {
		return OutputWithEvent{
			Event:           event,
			TaggingMetadata: taggingMetadata,
			Tags:            tags,
		}
	}
	return Output{
		TaggingMetadata: taggingMetadata,
		Tags:            tags,
	}
}
