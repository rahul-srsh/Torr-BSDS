import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as servicediscovery from 'aws-cdk-lib/aws-servicediscovery';

// ── Networking Construct ──
class NetworkingConstruct extends Construct {
  public readonly vpc: ec2.Vpc;

  constructor(scope: Construct, id: string) {
    super(scope, id);

    this.vpc = new ec2.Vpc(this, 'HopVaultVpc', {
      maxAzs: 2,
      subnetConfiguration: [
        {
          cidrMask: 24,
          name: 'Public',
          subnetType: ec2.SubnetType.PUBLIC,
        },
        {
          cidrMask: 24,
          name: 'Private',
          subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
        },
      ],
    });
  }
}

// ── Compute Construct ──
class ComputeConstruct extends Construct {
  public readonly cluster: ecs.Cluster;
  public readonly repositories: Record<string, ecr.Repository>;

  constructor(scope: Construct, id: string, vpc: ec2.Vpc) {
    super(scope, id);

    this.cluster = new ecs.Cluster(this, 'HopVaultCluster', {
      vpc,
      clusterName: 'hopvault-cluster',
    });

    const componentNames = [
      'directory-server',
      'guard-node',
      'relay-node',
      'exit-node',
      'echo-server',
    ];

    this.repositories = {};
    for (const name of componentNames) {
      this.repositories[name] = new ecr.Repository(this, `Repo-${name}`, {
        repositoryName: `hopvault/${name}`,
        removalPolicy: cdk.RemovalPolicy.DESTROY,
        emptyOnDelete: true,
      });
    }
  }
}

// ── Service Discovery Construct ──
class ServiceDiscoveryConstruct extends Construct {
  public readonly namespace: servicediscovery.PrivateDnsNamespace;

  constructor(scope: Construct, id: string, vpc: ec2.Vpc) {
    super(scope, id);

    this.namespace = new servicediscovery.PrivateDnsNamespace(this, 'HopVaultNamespace', {
      name: 'hopvault.local',
      vpc,
      description: 'Private DNS namespace for HopVault service discovery',
    });
  }
}

// ── Main Stack ──
export class InfraStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

    const networking = new NetworkingConstruct(this, 'Networking');
    const compute = new ComputeConstruct(this, 'Compute', networking.vpc);
    const discovery = new ServiceDiscoveryConstruct(this, 'ServiceDiscovery', networking.vpc);

    // Outputs
    new cdk.CfnOutput(this, 'VpcId', { value: networking.vpc.vpcId });
    new cdk.CfnOutput(this, 'ClusterName', { value: compute.cluster.clusterName });
    new cdk.CfnOutput(this, 'NamespaceId', { value: discovery.namespace.namespaceId });
  }
}
