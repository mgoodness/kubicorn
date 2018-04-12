// Copyright © 2017 The Kubicorn Authors
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

package resources

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/kubicorn/kubicorn/apis/cluster"
	"github.com/kubicorn/kubicorn/cloud"
	"github.com/kubicorn/kubicorn/pkg/compare"
	"github.com/kubicorn/kubicorn/pkg/logger"
)

var _ cloud.Resource = &PublicRouteTable{}

type PublicRouteTable struct {
	Shared
	ClusterPublicSubnet *cluster.PublicSubnet
}

func (r *PublicRouteTable) Actual(immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("publicroutetable.Actual")
	newResource := &PublicRouteTable{
		Shared: Shared{
			Name: r.Name,
			Tags: make(map[string]string),
		},
	}

	if r.ClusterPublicSubnet.Identifier != "" {
		input := &ec2.DescribeRouteTablesInput{
			Filters: []*ec2.Filter{
				{
					Name:   S("tag:kubicorn-public-route-table-subnet-pair"),
					Values: []*string{S(r.ClusterPublicSubnet.Name)},
				},
			},
		}
		output, err := Sdk.Ec2.DescribeRouteTables(input)
		if err != nil {
			return nil, nil, err
		}
		llc := len(output.RouteTables)
		if llc > 0 {
			rt := output.RouteTables[0]
			for _, tag := range rt.Tags {
				key := *tag.Key
				val := *tag.Value
				newResource.Tags[key] = val
			}
			newResource.Name = r.ClusterPublicSubnet.Name
			newResource.Identifier = r.ClusterPublicSubnet.Name
		}
	}
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *PublicRouteTable) Expected(immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("publicroutetable.Expected")
	newResource := &PublicRouteTable{
		Shared: Shared{
			Tags: map[string]string{
				"Name":                                    r.Name,
				"KubernetesCluster":                       immutable.Name,
				"kubicorn-public-route-table-subnet-pair": r.ClusterPublicSubnet.Name,
			},
			Name:       r.ClusterPublicSubnet.Name,
			Identifier: r.ClusterPublicSubnet.Name,
		},
	}
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *PublicRouteTable) Apply(actual, expected cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("publicroutetable.Apply")
	applyResource := expected.(*PublicRouteTable)
	isEqual, err := compare.IsEqual(actual.(*PublicRouteTable), expected.(*PublicRouteTable))
	if err != nil {
		return nil, nil, err
	}
	if isEqual {
		return immutable, applyResource, nil
	}

	// --- Create Public Route Table
	rtInput := &ec2.CreateRouteTableInput{
		VpcId: &immutable.ProviderConfig().Network.Identifier,
	}
	rtOutput, err := Sdk.Ec2.CreateRouteTable(rtInput)
	if err != nil {
		return nil, nil, err
	}
	logger.Success("Created Public Route Table [%s]", *rtOutput.RouteTable.RouteTableId)

	//  --- Lookup Internet Gateway
	input := &ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{
				Name:   S("tag:kubicorn-internet-gateway-name"),
				Values: []*string{S(immutable.Name)},
			},
		},
	}
	output, err := Sdk.Ec2.DescribeInternetGateways(input)
	if err != nil {
		return nil, nil, err
	}
	lsn := len(output.InternetGateways)
	if lsn != 1 {
		return nil, nil, fmt.Errorf("Found [%d] Internet Gateways for ID [%s]", lsn, r.ClusterPublicSubnet.Identifier)
	}
	ig := output.InternetGateways[0]
	logger.Info("Mapping Public Route Table [%s] to Internet Gateway [%s]", *rtOutput.RouteTable.RouteTableId, *ig.InternetGatewayId)

	// --- Map Public Route Table to Internet Gateway
	riInput := &ec2.CreateRouteInput{
		DestinationCidrBlock: S("0.0.0.0/0"),
		GatewayId:            ig.InternetGatewayId,
		RouteTableId:         rtOutput.RouteTable.RouteTableId,
	}
	_, err = Sdk.Ec2.CreateRoute(riInput)
	if err != nil {
		return nil, nil, err
	}

	subnetID := ""
	for _, psn := range immutable.ProviderConfig().Network.PublicSubnets {
		if psn.Name == r.Name {
			subnetID = psn.Identifier
		}
	}
	if subnetID == "" {
		return nil, nil, fmt.Errorf("Unable to find Public Subnet ID")
	}

	// --- Associate Route table to this particular Public Subnet
	asInput := &ec2.AssociateRouteTableInput{
		SubnetId:     &subnetID,
		RouteTableId: rtOutput.RouteTable.RouteTableId,
	}
	_, err = Sdk.Ec2.AssociateRouteTable(asInput)
	if err != nil {
		return nil, nil, err
	}

	logger.Success("Associated Route Table [%s] with Public Subnet [%s]", *rtOutput.RouteTable.RouteTableId, subnetID)
	newResource := &PublicRouteTable{}
	newResource.Identifier = *rtOutput.RouteTable.RouteTableId
	newResource.Name = applyResource.Name

	err = newResource.tag(applyResource.Tags)
	if err != nil {
		return nil, nil, fmt.Errorf("Unable to tag new Public Route Table: %v", err)
	}

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *PublicRouteTable) Delete(actual cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("publicroutetable.Delete")
	deleteResource := actual.(*PublicRouteTable)
	if deleteResource.Identifier == "" {
		return nil, nil, fmt.Errorf("Unable to delete Public Route Table resource without ID [%s]", deleteResource.Name)
	}
	input := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name:   S("tag:kubicorn-public-route-table-subnet-pair"),
				Values: []*string{S(r.ClusterPublicSubnet.Name)},
			},
		},
	}
	output, err := Sdk.Ec2.DescribeRouteTables(input)
	if err != nil {
		return nil, nil, err
	}
	llc := len(output.RouteTables)
	if llc != 1 {
		return nil, nil, fmt.Errorf("Found [%d] Public Route Tables for VPC ID [%s]", llc, r.ClusterPublicSubnet.Identifier)
	}
	rt := output.RouteTables[0]

	dainput := &ec2.DisassociateRouteTableInput{
		AssociationId: rt.Associations[0].RouteTableAssociationId,
	}
	_, err = Sdk.Ec2.DisassociateRouteTable(dainput)
	if err != nil {
		return nil, nil, err
	}

	dinput := &ec2.DeleteRouteTableInput{
		RouteTableId: rt.RouteTableId,
	}
	_, err = Sdk.Ec2.DeleteRouteTable(dinput)
	if err != nil {
		return nil, nil, err
	}
	logger.Success("Deleted Public Route Table [%s]", actual.(*PublicRouteTable).Identifier)

	newResource := &PublicRouteTable{}
	newResource.Name = actual.(*PublicRouteTable).Name
	newResource.Tags = actual.(*PublicRouteTable).Tags

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *PublicRouteTable) tag(tags map[string]string) error {
	logger.Debug("publicroutetable.Tag")
	tagInput := &ec2.CreateTagsInput{
		Resources: []*string{&r.Identifier},
	}
	for key, val := range tags {
		logger.Debug("Registering Public Route Table tag [%s] %s", key, val)
		tagInput.Tags = append(tagInput.Tags, &ec2.Tag{
			Key:   S("%s", key),
			Value: S("%s", val),
		})
	}
	_, err := Sdk.Ec2.CreateTags(tagInput)
	if err != nil {
		return err
	}
	return nil
}

func (r *PublicRouteTable) immutableRender(newResource cloud.Resource, inaccurateCluster *cluster.Cluster) *cluster.Cluster {
	logger.Debug("publicroutetable.Render")
	return inaccurateCluster
}
