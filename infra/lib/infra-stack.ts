import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as servicediscovery from 'aws-cdk-lib/aws-servicediscovery';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as path from 'path';

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

// ── Fargate Services Construct ──
interface ServiceDefinition {
  name: string;
  desiredCount: number;
}

class FargateServicesConstruct extends Construct {
  constructor(
    scope: Construct,
    id: string,
    props: {
      cluster: ecs.Cluster;
      namespace: servicediscovery.PrivateDnsNamespace;
      vpc: ec2.Vpc;
    }
  ) {
    super(scope, id);

    const services: ServiceDefinition[] = [
      { name: 'directory-server', desiredCount: 1 },
      { name: 'guard-node', desiredCount: 1 },
      { name: 'relay-node', desiredCount: 2 },
      { name: 'exit-node', desiredCount: 1 },
    ];

    // Security group shared by all services so they can talk to each other
    const servicesSg = new ec2.SecurityGroup(this, 'ServicesSg', {
      vpc: props.vpc,
      description: 'Allow all traffic between HopVault services',
      allowAllOutbound: true,
    });
    servicesSg.addIngressRule(servicesSg, ec2.Port.allTcp(), 'Allow inter-service traffic');

    for (const svc of services) {
      const taskDef = new ecs.FargateTaskDefinition(this, `TaskDef-${svc.name}`, {
        memoryLimitMiB: 512,
        cpu: 256,
      });

      taskDef.addContainer(`Container-${svc.name}`, {
        image: ecs.ContainerImage.fromAsset(path.join(__dirname, '../../'), {
          file: 'docker/Dockerfile.base',
        }),
        environment: {
          NODE_TYPE: svc.name,
          PORT: '8080',
          DIRECTORY_SERVER_URL: 'http://directory-server.hopvault.local:8080',
        },
        portMappings: [{ containerPort: 8080 }],
        logging: ecs.LogDrivers.awsLogs({
          streamPrefix: svc.name,
          logRetention: logs.RetentionDays.ONE_WEEK,
        }),
        healthCheck: {
          command: ['CMD-SHELL', 'wget -qO- http://localhost:8080/health || exit 1'],
          interval: cdk.Duration.seconds(10),
          timeout: cdk.Duration.seconds(5),
          startPeriod: cdk.Duration.seconds(10),
          retries: 3,
        },
      });

      new ecs.FargateService(this, `Service-${svc.name}`, {
        cluster: props.cluster,
        taskDefinition: taskDef,
        desiredCount: svc.desiredCount,
        assignPublicIp: false,
        securityGroups: [servicesSg],
        vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
        cloudMapOptions: {
          name: svc.name,
          cloudMapNamespace: props.namespace,
          dnsRecordType: servicediscovery.DnsRecordType.A,
          dnsTtl: cdk.Duration.seconds(10),
        },
        serviceName: svc.name,
      });
    }
  }
}

// ── Main Stack ──
export class InfraStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

    const networking = new NetworkingConstruct(this, 'Networking');
    const compute = new ComputeConstruct(this, 'Compute', networking.vpc);
    const discovery = new ServiceDiscoveryConstruct(this, 'ServiceDiscovery', networking.vpc);

    new FargateServicesConstruct(this, 'FargateServices', {
      cluster: compute.cluster,
      namespace: discovery.namespace,
      vpc: networking.vpc,
    });

    // Outputs
    new cdk.CfnOutput(this, 'VpcId', { value: networking.vpc.vpcId });
    new cdk.CfnOutput(this, 'ClusterName', { value: compute.cluster.clusterName });
    new cdk.CfnOutput(this, 'NamespaceId', { value: discovery.namespace.namespaceId });
  }
}