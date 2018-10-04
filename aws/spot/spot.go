package main

import (
	"github.com/aws/aws-sdk-go/aws"
	//	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	//	"math"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/kr/pretty"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	WAIT_COUNT = 60
)

type Vpc struct {
	Id string `json:"id"`
	Sg string `json:"sg"`
}

type Ami struct {
	Default string `json:"default"`
	Al2     string `json:"al2",omitempty`
}

type Region struct {
	Name    string `json:"name"`
	ExtName string `json:"ext-name"`
	Vpc     Vpc    `json:"vpc"`
	Ami     Ami    `json:"ami"`
	KeyPair string `json:"keypair"`
	Code    string `json:"code"`
}

type KeyFile struct {
	KeyPair string `json:"keypair"`
	KeyFile string `json:"keyfile"`
}

type UserDataStruct struct {
	Name string `json:"name"`
	File string `json:"file"`
}

type AWSRegions struct {
	Regions  []*Region         `json:"regions"`
	KeyFiles []*KeyFile        `json:"keyfiles"`
	UserData []*UserDataStruct `json:"userdata"`
}

type InstanceConfig struct {
	RegionName string `json:"region"`
	Type       string `json:"type"`
	Number     int    `json:"number"`
	AmiName    string `json:"ami",omitempty`
}

type LaunchConfig struct {
	RegionInstances []*InstanceConfig `json:"launch"`
	UserData        *UserDataStruct   `json:"userdata"`
	Batch           int               `json:"batch"`
}

var (
	whoami        = os.Getenv("WHOAMI")
	t             = time.Now()
	now           = fmt.Sprintf("%d-%02d-%02d_%02d_%02d_%02d", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
	configDir     = flag.String("config_dir", "../configs", "the directory of all the configuration files")
	launchProfile = flag.String("launch_profile", "launch-1k.json", "the profile name for instance launch")
	awsProfile    = flag.String("aws_profile", "aws.json", "the profile of the aws configuration")
	debug         = flag.Int("debug", 0, "enable debug output level")
	tag           = flag.String("tag", whoami, "a tag in instance name")

	userData = flag.String("user_data", "userdata.sh", "userdata file for instance launch")

	myInstances map[string]string = make(map[string]string)
	wg          sync.WaitGroup

	messages = make(chan string)
)

func exitErrorf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func parseAWSRegionConfig(config string) (*AWSRegions, error) {
	data, err := ioutil.ReadFile(config)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Can't read file (%v): %v", config, err))
	}
	if !json.Valid(data) {
		return nil, errors.New(fmt.Sprintf("Invalid Json data %s!", data))
	}
	regionConfig := new(AWSRegions)
	if err := json.Unmarshal(data, regionConfig); err != nil {
		return nil, errors.New(fmt.Sprintf("Can't parse AWs regions config (%v): %v", config, err))
	}
	return regionConfig, nil
}

func parseLaunchConfig(config string) (*LaunchConfig, error) {
	data, err := ioutil.ReadFile(config)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Can't read file (%v): %v", config, err))
	}
	if !json.Valid(data) {
		return nil, errors.New(fmt.Sprintf("Invalid Json data %s!", data))
	}
	launchConfig := new(LaunchConfig)
	if err := json.Unmarshal(data, launchConfig); err != nil {
		return nil, errors.New(fmt.Sprintf("Can't Unmarshal launch config (%v): %v", config, err))
	}
	return launchConfig, nil
}

func findAMI(region *Region, amiName string) (string, error) {
	switch amiName {
	case "al2":
		return region.Ami.Al2, nil
	default:
		return region.Ami.Default, nil
	}
	return "", fmt.Errorf("Can't find the right AMI: %v", amiName)
}

// return the struct pointer to the region based on name
func findRegion(regions *AWSRegions, name string) (*Region, error) {
	for _, r := range regions.Regions {
		if strings.Compare(r.Name, name) == 0 {
			return r, nil
		}
	}

	return nil, fmt.Errorf("Can't find the region: %v", name)
}

func findSubnet(svc *ec2.EC2, vpc Vpc) ([]*ec2.Subnet, error) {

	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("vpc-id"),
				Values: []*string{
					aws.String(vpc.Id),
				},
			},
		},
	}

	subnets, err := svc.DescribeSubnets(input)

	if err != nil {
		return nil, fmt.Errorf("DescribeSubnets Error: %v", err)
	}
	if *debug > 1 {
		fmt.Printf("Subnet output: %v", pretty.Formatter(subnets))
	}

	return subnets.Subnets, nil
}

func launchSpotInstances(reg *Region, i *InstanceConfig) error {
	defer wg.Done()

	messages <- fmt.Sprintf("launching spot instances in region: %v\n", reg.Name)

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(reg.ExtName)},
	)
	if err != nil {
		messages <- fmt.Sprintf("%v: aws session Error: %v", reg.Name, err)
		return fmt.Errorf("aws session Error: %v", err)
	}

	// Create an EC2 service client.
	svc := ec2.New(sess)

	amiId, err := findAMI(reg, i.AmiName)
	if err != nil {
		messages <- fmt.Sprintf("%v: findAMI Error %v", reg.Name, err)
		return fmt.Errorf("findAMI Error %v", err)
	}

	var reservations []*string
	input := ec2.RunInstancesInput{
		ImageId:          aws.String(amiId),
		InstanceType:     aws.String(i.Type),
		MinCount:         aws.Int64(int64(i.Number / 2)),
		MaxCount:         aws.Int64(int64(i.Number)),
		KeyName:          aws.String(reg.KeyPair),
		SecurityGroupIds: []*string{aws.String(reg.Vpc.Sg)},
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(fmt.Sprintf("%s-%s-%s-1", reg.Code, whoami, now)),
					},
				},
			},
		},
	}

	if *debug > 1 {
		fmt.Printf("input: %v\n", pretty.Formatter(input))
	}

	reservation, err := svc.RunInstances(&input)

	if err != nil {
		return err
	}

	if *debug > 0 {
		fmt.Println(pretty.Formatter(reservation))
	}

	reservations = append(reservations, reservation.ReservationId)

	instanceInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("reservation-id"),
				Values: reservations,
			},
		},
	}

	messages <- fmt.Sprintf("%v: sleeping for %d seconds\n", reg.Name, WAIT_COUNT)
	time.Sleep(WAIT_COUNT * time.Second)

	for m := 0; m < WAIT_COUNT; m++ {
		result, err := svc.DescribeInstances(instanceInput)

		if err != nil {
			fmt.Errorf("DescribeInstances Error: %v", err)
			break
		}

		/*
			token := result.NextToken
			if *debug > 2 {
				fmt.Printf("describe instances next token: %v\n", token)
			}
		*/
		for _, r := range result.Reservations {
			for _, inst := range r.Instances {
				if *inst.PublicIpAddress != "" {
					if _, ok := myInstances[*inst.PublicIpAddress]; !ok {
						myInstances[*inst.PublicIpAddress] = *inst.PublicDnsName
					}
				} else {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}

	if *debug > 1 {
		for ip, name := range myInstances {
			fmt.Printf("[%v] => [%v]\n", ip, name)
		}
	}
	messages <- fmt.Sprintf("%v: %d instances", reg.Name, len(myInstances))
	return nil
}

func main() {
	flag.Parse()

	regions, err := parseAWSRegionConfig(filepath.Join(*configDir, *awsProfile))
	if err != nil {
		exitErrorf("Exiting ... : %v", err)
	}
	if *debug > 1 {
		fmt.Printf("regions: %# v\n", pretty.Formatter(regions))
	}

	launches, err := parseLaunchConfig(filepath.Join(*configDir, *launchProfile))
	if err != nil {
		exitErrorf("Exiting ... : %v", err)
	}
	if *debug > 0 {
		fmt.Printf("launch: %# v\n", pretty.Formatter(launches))
	}

	var userDataString string
	if data, err := ioutil.ReadFile(launches.UserData.File); err != nil {
		exitErrorf("Unable to read userdata file: %v", launches.UserData.File)
	} else {
		// encode userData
		userDataString = base64.StdEncoding.EncodeToString(data)
	}

	if *debug > 2 {
		fmt.Printf("userdata encoded: %# v\n", pretty.Formatter(userDataString))
		data, err := base64.StdEncoding.DecodeString(userDataString)
		if err == nil {
			fmt.Printf("userdata decoded: %q\n", data)
		}
	}

	for _, r := range launches.RegionInstances {
		wg.Add(1)
		region, err := findRegion(regions, r.RegionName)
		if err != nil {
			exitErrorf("findRegion Error: %v", err)
		}
		go launchSpotInstances(region, r)
	}
	go func() {
		for i := range messages {
			fmt.Println(i)
		}
	}()
	wg.Wait()
}